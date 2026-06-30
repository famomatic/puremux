package mp4

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/famomatic/puremux/internal/core"
)

// Reader decodes an MP4/M4A file into tracks and samples. It walks the ISO
// Base Media File Format (ISO/IEC 14496-12) box tree, reads moov/trak
// metadata + sample tables, then yields opaque media samples by seeking into
// mdat. NO media decoding is performed; sample payloads are copied verbatim
// (ARCHITECTURE.md section 4 opacity invariant).
//
// MP4 sample tables reference absolute mdat offsets, so the input must be an
// io.ReadSeeker.
type Reader struct {
	rs     io.ReadSeeker
	tracks []*trackState
	// inited is set once the streaming cursors below are primed.
	inited bool
}

type trackState struct {
	info       Track
	timescale  uint32
	sampleSize []uint32 // stsz per-sample sizes
	uniform    uint32   // stsz uniform sample size (0 if per-sample)
	stts       []sttsEntry
	stsc       []stscEntry
	stco       []uint64 // chunk offsets (stco 32-bit or co64 64-bit)
	stss       []uint32 // sync sample numbers (1-based); nil = unknown

	// --- Streaming cursors (O(1) memory, independent of sample count) ---
	totalSamples uint32 // total samples in the track
	consumed     uint32 // samples already emitted (0-based next sample number)

	// stts cursor: time accumulation
	sttsIdx    int    // current stts entry index
	sttsLeft   uint32 // samples remaining in the current stts entry
	accumUnits uint64 // accumulated timescale units up to the NEXT sample

	// chunk cursor: offset accumulation
	chunkIdx        int    // current chunk index into stco
	chunkSampleLeft uint32 // samples remaining in the current chunk
	chunkOff        int64  // file offset of the NEXT sample within the current chunk

	// stss cursor: sync-sample lookup
	stssIdx int // index into stss for the next sync sample >= consumed+1

	// peeked sample (computed by currentSample, consumed by advanceSample)
	peek    samplePeek
	hasPeek bool
}

type sttsEntry struct {
	count uint32
	delta uint32
}

type stscEntry struct {
	firstChunk      uint32
	samplesPerChunk uint32
	sampleDescIndex uint32
}

// samplePeek is a lazily-computed view of the next sample to emit. It holds
// only the current sample's metadata, not the whole table.
type samplePeek struct {
	absMs    uint64
	keyframe bool
	off      int64
	size     uint32
}

// ErrNotSeekable is returned when the MP4 input is not an io.ReadSeeker.
var ErrNotSeekable = errors.New("mp4: input must be seekable (io.ReadSeeker)")

// ErrCorrupt is returned for a structurally invalid MP4 file.
var ErrCorrupt = errors.New("mp4: corrupt or unsupported file")

// box is a parsed box header.
type box struct {
	typ     string
	size    int64
	hdrSize int
	payload int64
}

// readBox reads the next box header from r at its current position.
// size==0 means "box extends to EOF"; size==1 means a 64-bit largesize follows.
func readBox(r io.Reader) (box, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return box{}, err
	}
	b := box{
		typ:     string(hdr[4:8]),
		size:    int64(binary.BigEndian.Uint32(hdr[0:4])),
		hdrSize: 8,
	}
	if b.size == 1 {
		var ls [8]byte
		if _, err := io.ReadFull(r, ls[:]); err != nil {
			return box{}, err
		}
		b.size = int64(binary.BigEndian.Uint64(ls[:]))
		b.hdrSize = 16
	}
	if b.size > 0 {
		b.payload = b.size - int64(b.hdrSize)
	} else {
		b.payload = -1 // extends to EOF
	}
	return b, nil
}

func skipBox(r io.Reader, b box) error {
	if b.payload < 0 {
		return io.EOF
	}
	_, err := io.CopyN(io.Discard, r, b.payload)
	return err
}

// Tracks returns the parsed track descriptors.
func (rd *Reader) Tracks() []Track {
	out := make([]Track, 0, len(rd.tracks))
	for _, t := range rd.tracks {
		out = append(out, t.info)
	}
	return out
}

// codecFromSampleEntry maps an MP4 sample entry type to a codec + track kind.
func codecFromSampleEntry(typ string, number int) Track {
	t := Track{Number: number}
	switch strings.TrimSpace(typ) {
	case "av01", "av1C":
		t.Codec = core.CodecAV1
		t.IsVideo = true
	case "vp09", "vpcC":
		t.Codec = core.CodecVP9
		t.IsVideo = true
	case "vp08":
		t.Codec = core.CodecVP8
		t.IsVideo = true
	case "avc1", "avc3":
		t.Codec = core.CodecH264
		t.IsVideo = true
	case "hvc1", "hev1", "hvc3", "hev3":
		t.Codec = core.CodecHEVC
		t.IsVideo = true
	case "Opus", "opus":
		t.Codec = core.CodecOpus
		t.Channels = 2
		t.SampleRate = 48000
	case "fLaC", "flac":
		t.Codec = core.CodecFLAC
		t.Channels = 2
	case "mp4a":
		t.Codec = core.CodecAAC
		t.Channels = 2
		t.SampleRate = 48000
	default:
		t.Codec = core.CodecUnknown
	}
	return t
}

// timescaleToMs converts a timescale-unit timestamp to milliseconds.
func timescaleToMs(units uint64, timescale uint32) uint64 {
	if timescale == 0 {
		return 0
	}
	return (units*1000 + uint64(timescale)/2) / uint64(timescale)
}

// samplesPerChunkFor resolves the stsc table for a 1-based chunk number.
func samplesPerChunkFor(stsc []stscEntry, chunk uint32) uint32 {
	if len(stsc) == 0 {
		return 1
	}
	spc := stsc[len(stsc)-1].samplesPerChunk
	for i := len(stsc) - 1; i >= 0; i-- {
		if chunk >= stsc[i].firstChunk {
			spc = stsc[i].samplesPerChunk
			break
		}
	}
	if spc == 0 {
		spc = 1
	}
	return spc
}
