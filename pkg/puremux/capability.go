// Package puremux capability/gate layer.
//
// This file declares the container/codec compatibility surface of puremux
// and the cheap, file-header-sniffing gates that let a caller decide whether
// puremux can handle an (input, output) pair BEFORE attempting a full remux.
//
// The gates exist because the alternative - parse the whole input and fail
// deep inside the EBML/MP4 reader - is slow and produces opaque errors that
// force a downstream FallbackMuxer into expensive exception-based recovery.
// A 4-byte magic sniff resolves the decision in O(1) without touching any
// codec packet payload, so the opacity invariant (ARCHITECTURE.md section 4)
// is preserved.
package puremux

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/famomatic/puremux/internal/core"
)

// Container is a muxing/demuxing container format puremux understands.
type Container uint8

const (
	// ContainerUnknown is the zero value; used when sniffing fails.
	ContainerUnknown Container = iota
	// ContainerWebM is the WebM container (Matroska subset; EBML header
	// DocType == "webm"). Restricted codec set: VP8/VP9/AV1 video, Opus/Vorbis audio.
	ContainerWebM
	// ContainerMKV is the Matroska container (EBML header DocType == "matroska").
	// Strict superset of WebM; additionally permits FLAC audio and other Matroska codecs.
	ContainerMKV
	// ContainerMP4 is the ISO Base Media File Format container (ftyp box).
	// Input only: puremux reads MP4 to remux into WebM/MKV. It never writes MP4.
	ContainerMP4
)

// String returns the lowercase container name.
func (c Container) String() string {
	switch c {
	case ContainerWebM:
		return "webm"
	case ContainerMKV:
		return "mkv"
	case ContainerMP4:
		return "mp4"
	default:
		return "unknown"
	}
}

// Extension returns the canonical file extension for the container, without
// the leading dot.
func (c Container) Extension() string {
	switch c {
	case ContainerWebM:
		return "webm"
	case ContainerMKV:
		return "mkv"
	case ContainerMP4:
		return "mp4"
	default:
		return ""
	}
}

// IsEBML reports whether the container uses the EBML framing (WebM and MKV).
func (c Container) IsEBML() bool { return c == ContainerWebM || c == ContainerMKV }

// CodecCombo is one supported (codec, track-kind) entry for a container.
type CodecCombo struct {
	Codec core.CodecType
	Kind  core.TrackKind
}

// ContainerCapability describes what puremux can do with a container.
type ContainerCapability struct {
	Container Container
	// CanRead is true when puremux has a demuxer (reader) for the container.
	CanRead bool
	// CanWrite is true when puremux has a muxer (writer) for the container.
	CanWrite bool
	// Codecs lists the codecs puremux supports inside this container.
	Codecs []CodecCombo
}

// ErrUnsupportedInput is returned when the input container/codec is not
// readable by puremux. Callers should fall back to an external muxer.
var ErrUnsupportedInput = errors.New("puremux: unsupported input format")

// ErrUnsupportedOutput is returned when the output container/codec is not
// writable by puremux.
var ErrUnsupportedOutput = errors.New("puremux: unsupported output format")

// ErrIncompatible is returned when puremux can read the input and write the
// output, but the input codecs are not all expressible in the output container.
var ErrIncompatible = errors.New("puremux: input codecs not expressible in output container")

// webMCodecs are the codecs permitted in a WebM container (Matroska subset
// restriction). VP8/VP9/AV1 video; Opus audio; Vorbis is permitted by the
// WebM spec for legacy streams.
var webMCodecs = []CodecCombo{
	{Codec: core.CodecVP8, Kind: core.TrackVideo},
	{Codec: core.CodecVP9, Kind: core.TrackVideo},
	{Codec: core.CodecAV1, Kind: core.TrackVideo},
	{Codec: core.CodecOpus, Kind: core.TrackAudio},
	{Codec: core.CodecVorbis, Kind: core.TrackAudio},
}

// mkvCodecs are the codecs permitted in a Matroska (MKV) container. It is a
// strict superset of WebM codecs, adding FLAC lossless audio.
var mkvCodecs = []CodecCombo{
	{Codec: core.CodecVP8, Kind: core.TrackVideo},
	{Codec: core.CodecVP9, Kind: core.TrackVideo},
	{Codec: core.CodecAV1, Kind: core.TrackVideo},
	{Codec: core.CodecOpus, Kind: core.TrackAudio},
	{Codec: core.CodecVorbis, Kind: core.TrackAudio},
	{Codec: core.CodecFLAC, Kind: core.TrackAudio},
	{Codec: core.CodecH264, Kind: core.TrackVideo},
	{Codec: core.CodecHEVC, Kind: core.TrackVideo},
}

// SupportedInputs returns the containers puremux can read (demux). The same
// codec set applies to read and write for EBML containers.
func SupportedInputs() []ContainerCapability {
	return []ContainerCapability{
		{Container: ContainerWebM, CanRead: true, CanWrite: false, Codecs: webMCodecs},
		{Container: ContainerMKV, CanRead: true, CanWrite: false, Codecs: mkvCodecs},
		{Container: ContainerMP4, CanRead: true, CanWrite: false, Codecs: mp4Codecs},
	}
}

// SupportedOutputs returns the containers puremux can write (mux). puremux
// never writes MP4; MP4 is input-only.
func SupportedOutputs() []ContainerCapability {
	return []ContainerCapability{
		{Container: ContainerWebM, CanRead: false, CanWrite: true, Codecs: webMCodecs},
		{Container: ContainerMKV, CanRead: false, CanWrite: true, Codecs: mkvCodecs},
	}
}

// mp4Codecs are the codecs puremux can demux from an MP4 container. AV1 and
// VP9 are delivered by YouTube via MP4; Opus is the audio. VP8 is uncommon in
// MP4 and is omitted.
var mp4Codecs = []CodecCombo{
	{Codec: core.CodecVP9, Kind: core.TrackVideo},
	{Codec: core.CodecAV1, Kind: core.TrackVideo},
	{Codec: core.CodecOpus, Kind: core.TrackAudio},
	{Codec: core.CodecAAC, Kind: core.TrackAudio},
	{Codec: core.CodecFLAC, Kind: core.TrackAudio},
	{Codec: core.CodecH264, Kind: core.TrackVideo},
	{Codec: core.CodecHEVC, Kind: core.TrackVideo},
}

// codecsForContainer returns the permitted codec list for a container, or nil
// if the container is unknown.
func codecsForContainer(c Container) []CodecCombo {
	switch c {
	case ContainerWebM:
		return webMCodecs
	case ContainerMKV:
		return mkvCodecs
	case ContainerMP4:
		return mp4Codecs
	default:
		return nil
	}
}

// codecAllowed reports whether codec is permitted in the given container.
func codecAllowed(c Container, codec core.CodecType) bool {
	for _, cc := range codecsForContainer(c) {
		if cc.Codec == codec {
			return true
		}
	}
	return false
}

// DetectContainer sniffs the leading bytes of a file and returns the
// container format. It reads at most 64 bytes. It does NOT parse any codec
// payload; only the container framing magic is inspected (§4 opacity safe).
//
// Detection rules:
//   - EBML header magic 0x1A 0x45 0xDF 0xA3 -> WebM or MKV (doctype sniffed).
//   - MP4 ftyp box: bytes 4..7 == "ftyp" (or "styp" for fragmented).
func DetectContainer(path string) (Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return ContainerUnknown, err
	}
	defer f.Close()
	return DetectContainerReader(f.Name(), f)
}

// DetectContainerReader sniffs an io.Reader (which must be the start of the
// media file) and returns the container. The name hint is used only to break
// the WebM/MKV tie when the doctype cannot be parsed (it falls back to the
// extension).
func DetectContainerReader(name string, r io.Reader) (Container, error) {
	buf := make([]byte, 64)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return ContainerUnknown, err
	}
	if n < 4 {
		return ContainerUnknown, ErrUnsupportedInput
	}
	// EBML header magic: 0x1A 0x45 0xDF 0xA3 (idEBML Element ID).
	if buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3 {
		return detectEBMLContainer(name, buf[:n]), nil
	}
	// MP4 ftyp/styp box: the box type is the 4 ASCII bytes at offset 4.
	if n >= 8 && (string(buf[4:8]) == "ftyp" || string(buf[4:8]) == "styp") {
		return ContainerMP4, nil
	}
	return ContainerUnknown, ErrUnsupportedInput
}

// detectEBMLContainer distinguishes WebM from MKV by reading the DocType string
// from the EBML header. If the doctype is unreadable, it falls back to the
// file extension hint. EBML header layout:
//
//	1A 45 DF A3  <size>  42 82 <size> <doctype-ascii>  ...
//	  idEBML              idDocType
//
// We walk only the header framing bytes; no codec payload is touched.
func detectEBMLContainer(name string, b []byte) Container {
	off := 4 // skip idEBML
	// skip EBML element size VINT
	w := ebmlVINTWidth(b[off])
	if w == 0 || off+w > len(b) {
		return ebmlByExtension(name)
	}
	off += w
	// Walk EBML header children until we find DocType (0x4282) or run out.
	for off+2 < len(b) {
		id, iw := decodeChildID(b[off:])
		if iw == 0 || off+iw >= len(b) {
			break
		}
		off += iw
		sz, sw := decodeVINT(b[off:])
		if sw == 0 || off+sw >= len(b) {
			break
		}
		off += sw
		if id == 0x4282 && int(sz) <= len(b)-off { // idDocType
			dt := string(b[off : off+int(sz)])
			switch dt {
			case "webm":
				return ContainerWebM
			case "matroska":
				return ContainerMKV
			}
		}
		off += int(sz)
	}
	return ebmlByExtension(name)
}

// ebmlByExtension guesses WebM vs MKV from the file extension only.
func ebmlByExtension(name string) Container {
	switch strings.ToLower(extensionOf(name)) {
	case "webm":
		return ContainerWebM
	case "mkv", "mka":
		return ContainerMKV
	default:
		return ContainerMKV // EBML but unknown doctype: treat as MKV superset.
	}
}

func extensionOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i+1:]
}

// CanRemux reports whether puremux has the readers and writers to attempt a
// remux from the given input container to the given output container.
//
// This is a coarse, container-level gate: it returns true when puremux has a
// demuxer for in and a muxer for out. It does NOT inspect packet payloads and
// does NOT pre-judge codec compatibility, because an input container may
// carry several codecs and only the actual tracks matter. Per-track codec
// compatibility is enforced later at AddTrack time (every track's codec must
// be expressible in the output container).
//
// Use CanRemux to decide whether to even open the files; use the AddTrack
// error to decide per-track rejection.
func CanRemux(in, out Container) bool {
	return isReadableInput(in) && isWritableOutput(out)
}

// CanRemuxCodecs reports whether every codec in the given set is expressible
// in the output container. This is the finer gate used once the input's actual
// tracks (and their codecs) are known. An empty codec set returns true.
func CanRemuxCodecs(out Container, codecs []core.CodecType) bool {
	if len(codecs) == 0 {
		return true
	}
	for _, c := range codecs {
		if !codecAllowed(out, c) {
			return false
		}
	}
	return true
}

// ebmlVINTWidth returns the EBML VINT width (1..8) implied by the leading
// marker bit of a byte. 0 means invalid.
func ebmlVINTWidth(b byte) int {
	for i := 0; i < 8; i++ {
		if b&(0x80>>uint(i)) != 0 {
			return i + 1
		}
	}
	return 0
}

// decodeChildID reads a 1..4 byte EBML Element ID from b, returning id and width.
func decodeChildID(b []byte) (uint32, int) {
	if len(b) == 0 {
		return 0, 0
	}
	w := ebmlVINTWidth(b[0])
	if w == 0 || w > 4 || len(b) < w {
		return 0, 0
	}
	var v uint32
	for i := 0; i < w; i++ {
		v = (v << 8) | uint32(b[i])
	}
	return v, w
}

// decodeVINT reads an EBML size VINT from b, returning value and width.
func decodeVINT(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	w := ebmlVINTWidth(b[0])
	if w == 0 || len(b) < w {
		return 0, 0
	}
	mask := byte(0xFF >> uint(w))
	var v uint64
	v |= uint64(b[0]&mask) << uint(8*(w-1))
	for i := 1; i < w; i++ {
		v |= uint64(b[i]) << uint(8*(w-1-i))
	}
	return v, w
}
