package puremux

import (
	"io"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// InputBlock is a container-agnostic decoded block emitted by an input reader.
// It mirrors webm.Block but decouples the merge loop from the EBML reader so
// the same loop can drive MP4 (and future) demuxers. Payloads stay opaque
// (ARCHITECTURE.md section 4).
type InputBlock struct {
	TrackNum int
	// AbsMs is the block's absolute timecode in milliseconds.
	AbsMs uint64
	// Keyframe is true for sync frames (video). Audio readers set false.
	Keyframe bool
	Data     []byte
}

// AbsTimecode returns the block's absolute timecode in milliseconds.
func (b *InputBlock) AbsTimecode() uint64 { return b.AbsMs }

// Duration returns the block's absolute timecode as a time.Duration.
func (b *InputBlock) Duration() time.Duration {
	return time.Duration(b.AbsMs) * time.Millisecond
}

// InputTrack is a container-agnostic track descriptor emitted by an input
// reader, in the same shape webm.ReadTrack already provides.
type InputTrack struct {
	Number       int
	Codec        core.CodecType
	IsVideo      bool
	Width        int
	Height       int
	Channels     int
	SampleRate   float64
	CodecPrivate []byte
}

// inputReader is the common demuxer interface the merge loop drives. Each
// container format (webm, mp4) provides an adapter implementing it.
type inputReader interface {
	// Tracks returns the parsed track descriptors.
	Tracks() []InputTrack
	// NextBlock returns the next block, or io.EOF when the stream is exhausted.
	NextBlock() (*InputBlock, error)
	// Close releases any resources held by the reader.
	Close() error
}

// openInputReader opens the input file and returns the appropriate demuxer
// based on the detected container. The container was already sniffed by the
// gate layer; we dispatch here without re-sniffing.
func openInputReader(path string, c Container) (inputReader, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	switch c {
	case ContainerWebM, ContainerMKV:
		return newWebMReader(f)
	case ContainerMP4:
		return newMP4Reader(f)
	default:
		_ = f.Close()
		return nil, ErrUnsupportedInput
	}
}

// trackCodec returns the codec for a track number, or CodecUnknown.
func trackCodec(tracks []InputTrack, num int) core.CodecType {
	for _, t := range tracks {
		if t.Number == num {
			return t.Codec
		}
	}
	return core.CodecUnknown
}

// closeAll closes r ignoring nil entries, used for cleanup on error.
func closeAll(rs ...io.Closer) {
	for _, r := range rs {
		if r != nil {
			_ = r.Close()
		}
	}
}
