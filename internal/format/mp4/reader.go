package mp4

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/famomatic/puremux/internal/core"
)

// Track is a track parsed from the input MP4 moov/trak box.
type Track struct {
	Number     int
	Codec      core.CodecType
	IsVideo    bool
	Width      int
	Height     int
	Channels   int
	SampleRate float64
}

// Sample is a decoded media sample from the input, equivalent to a Block.
type Sample struct {
	TrackNum int
	// AbsMs is the sample's absolute presentation time in milliseconds.
	AbsMs    uint64
	Keyframe bool
	Data     []byte
}

// NewReader wraps an MP4 input stream. The reader must implement
// io.ReadSeeker; MP4 sample tables reference absolute mdat offsets.
func NewReader(r io.Reader) (*Reader, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return nil, ErrNotSeekable
	}
	rd := &Reader{rs: rs}
	if err := rd.parse(); err != nil {
		return nil, err
	}
	if len(rd.tracks) == 0 {
		return nil, ErrCorrupt
	}
	return rd, nil
}

// NextSample returns the next media sample in merged absolute-time order
// across all tracks, or io.EOF when exhausted.
func (rd *Reader) NextSample() (*Sample, error) {
	// Prime streaming cursors on first call (O(tracks) work, no sample array).
	if !rd.inited {
		if err := rd.initCursors(); err != nil {
			return nil, err
		}
		rd.inited = true
	}
	// Find the track whose next (peeked) sample has the smallest absolute time.
	pick := -1
	var bestMs uint64
	for i, t := range rd.tracks {
		// Lazily compute the peek for this track if missing.
		if !t.hasPeek {
			if !t.peekNext() {
				continue // track exhausted
			}
			t.hasPeek = true
		}
		ms := t.peek.absMs
		if pick < 0 || ms < bestMs {
			pick = i
			bestMs = ms
		}
	}
	if pick < 0 {
		return nil, io.EOF
	}
	t := rd.tracks[pick]
	s := t.peek
	// Advance the cursor past the peeked sample so the next peek is fresh.
	t.hasPeek = false
	t.consumed++
	t.advancePast(s)
	if _, err := rd.rs.Seek(s.off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, s.size)
	if _, err := io.ReadFull(rd.rs, buf); err != nil {
		return nil, err
	}
	return &Sample{
		TrackNum: t.info.Number,
		AbsMs:    s.absMs,
		Keyframe: s.keyframe,
		Data:     buf,
	}, nil
}

// parse walks the top-level boxes (ftyp, moov, mdat).
func (rd *Reader) parse() error {
	for {
		off, _ := rd.rs.Seek(0, io.SeekCurrent)
		b, err := readBox(rd.rs)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		switch b.typ {
		case "ftyp", "styp", "free", "skip":
			if err := skipBox(rd.rs, b); err != nil {
				return err
			}
		case "moov":
			if err := rd.parseMoov(b); err != nil {
				return err
			}
		case "mdat":
			// Sample offsets are absolute file offsets (stco points into mdat
			// payload), so we only need to skip past mdat here.
			if err := skipBox(rd.rs, b); err != nil {
				return err
			}
		default:
			if err := skipBox(rd.rs, b); err != nil {
				return err
			}
		}
		_ = off
	}
	if len(rd.tracks) == 0 {
		return ErrCorrupt
	}
	return nil
}

// parseMoov reads moov fully and walks its children (mvhd, trak).
func (rd *Reader) parseMoov(b box) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(rd.rs, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "trak":
			if err := rd.parseTrak(mr, cb); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseTrak reads a trak box fully and walks tkhd/mdia.
func (rd *Reader) parseTrak(r io.Reader, b box) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	tr := bytes.NewReader(buf)
	t := &trackState{info: Track{Number: len(rd.tracks) + 1}}
	for tr.Len() > 0 {
		cb, err := readBox(tr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "tkhd":
			if err := skipBox(tr, cb); err != nil {
				return err
			}
		case "mdia":
			if err := rd.parseMdia(tr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(tr, cb); err != nil {
				return err
			}
		}
	}
	if t.timescale == 0 {
		return nil // no mdhd; skip
	}
	rd.tracks = append(rd.tracks, t)
	return nil
}

// parseMdia walks mdhd + minf.
func (rd *Reader) parseMdia(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "mdhd":
			if err := rd.parseMdhd(mr, cb, t); err != nil {
				return err
			}
		case "minf":
			if err := rd.parseMinf(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseMdhd reads the media timescale (4 bytes after version/flags + times).
func (rd *Reader) parseMdhd(r io.Reader, b box, t *trackState) error {
	// fullbox: version(1)+flags(3) = 4 bytes, then the mdhd body.
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	version := hdr[0]
	rest := int64(b.payload) - 4
	// creation + modification (8 bytes v0, 16 bytes v1).
	var skipN int64 = 8
	if version == 1 {
		skipN = 16
	}
	if _, err := io.CopyN(io.Discard, r, skipN); err != nil {
		return err
	}
	rest -= skipN
	var ts [4]byte
	if _, err := io.ReadFull(r, ts[:]); err != nil {
		return err
	}
	t.timescale = binary.BigEndian.Uint32(ts[:])
	rest -= 4
	// duration (8 bytes v1, 4 bytes v0). Read explicitly so the remaining
	// body (language + predefined) is skipped from the correct offset.
	var dur int64 = 4
	if version == 1 {
		dur = 8
	}
	if _, err := io.CopyN(io.Discard, r, dur); err != nil {
		return err
	}
	rest -= dur
	if rest > 0 {
		if _, err := io.CopyN(io.Discard, r, rest); err != nil {
			return err
		}
	}
	return nil
}

// parseMinf walks straight to stbl.
func (rd *Reader) parseMinf(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "stbl":
			if err := rd.parseStbl(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseStbl walks the sample table boxes.
func (rd *Reader) parseStbl(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "stsd":
			if err := rd.parseStsd(mr, cb, t); err != nil {
				return err
			}
		case "stts":
			if err := rd.parseStts(mr, cb, t); err != nil {
				return err
			}
		case "stsz":
			if err := rd.parseStsz(mr, cb, t); err != nil {
				return err
			}
		case "stsc":
			if err := rd.parseStsc(mr, cb, t); err != nil {
				return err
			}
		case "stco":
			if err := rd.parseStco(mr, cb, t, false); err != nil {
				return err
			}
		case "co64":
			if err := rd.parseStco(mr, cb, t, true); err != nil {
				return err
			}
		case "stss":
			if err := rd.parseStss(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseStsd(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	entryCount := binary.BigEndian.Uint32(hdr[4:8])
	if entryCount == 0 {
		remaining := int64(b.payload) - 8
		if remaining > 0 {
			if _, err := io.CopyN(io.Discard, r, remaining); err != nil {
				return err
			}
		}
		return nil
	}
	var se [8]byte
	if _, err := io.ReadFull(r, se[:]); err != nil {
		return err
	}
	codecType := string(se[4:8])
	t.info = codecFromSampleEntry(codecType, t.info.Number)
	remaining := int64(b.payload) - 8 - 8
	if remaining > 0 {
		if _, err := io.CopyN(io.Discard, r, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (rd *Reader) parseStts(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stts = append(t.stts, sttsEntry{
			count: binary.BigEndian.Uint32(e[0:4]),
			delta: binary.BigEndian.Uint32(e[4:8]),
		})
	}
	return nil
}

func (rd *Reader) parseStsz(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	t.uniform = binary.BigEndian.Uint32(hdr[4:8])
	count := binary.BigEndian.Uint32(hdr[8:12])
	if t.uniform != 0 {
		// Uniform sizes; per-sample table omitted. Skip any leftover.
		leftover := int64(b.payload) - 12
		if leftover > 0 {
			if _, err := io.CopyN(io.Discard, r, leftover); err != nil {
				return err
			}
		}
		return nil
	}
	for i := uint32(0); i < count; i++ {
		var e [4]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.sampleSize = append(t.sampleSize, binary.BigEndian.Uint32(e[:]))
	}
	return nil
}

func (rd *Reader) parseStsc(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [12]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stsc = append(t.stsc, stscEntry{
			firstChunk:      binary.BigEndian.Uint32(e[0:4]),
			samplesPerChunk: binary.BigEndian.Uint32(e[4:8]),
			sampleDescIndex: binary.BigEndian.Uint32(e[8:12]),
		})
	}
	return nil
}

func (rd *Reader) parseStco(r io.Reader, b box, t *trackState, co64 bool) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		if co64 {
			var e [8]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				return err
			}
			t.stco = append(t.stco, binary.BigEndian.Uint64(e[:]))
		} else {
			var e [4]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				return err
			}
			t.stco = append(t.stco, uint64(binary.BigEndian.Uint32(e[:])))
		}
	}
	return nil
}

func (rd *Reader) parseStss(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [4]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stss = append(t.stss, binary.BigEndian.Uint32(e[:]))
	}
	return nil
}
