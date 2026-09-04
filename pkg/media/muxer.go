package media

import (
	"context"
	"io"
	"time"
)

// MP4Mode selects the physical MP4 layout. Auto writes a progressive file
// when the destination is seekable and fragmented MP4 otherwise.
type MP4Mode uint8

const (
	MP4ModeAuto MP4Mode = iota
	MP4ModeProgressive
	MP4ModeFragmented
)

// MuxOptions configure a compressed-packet muxer. Format is mandatory.
// FragmentDuration and MaxFragmentBytes bound fMP4 buffering; zero selects
// conservative defaults.
type MuxOptions struct {
	Format           Format
	MP4Mode          MP4Mode
	FragmentDuration time.Duration
	MaxFragmentBytes int
}

// Muxer serializes already ordered compressed packets. It never repairs,
// reorders, or synthesizes timestamps.
//
// AddStream must be called before the first WritePacket. The returned index is
// the StreamIndex accepted by WritePacket. PTS, DTS, and Duration remain signed
// integer ticks in the registered Stream.TimeBase.
//
// WritePacket does not take ownership of p. Once it returns, the caller may
// release or reuse p and its Data. Implementations that buffer packets must
// copy retained bytes before returning.
type Muxer interface {
	AddStream(Stream) (int, error)
	WritePacket(context.Context, *Packet) error
	Close() error
}

// seekableWriter is the destination capability needed by progressive MP4.
// It is deliberately private: callers pass an ordinary io.Writer and the
// factory selects behavior from the implemented capabilities.
type seekableWriter interface {
	io.Writer
	io.Seeker
}

func validateMuxOptions(w io.Writer, opts MuxOptions) error {
	if w == nil {
		return ErrInvalidData
	}
	if opts.Format == FormatUnknown {
		return ErrUnsupportedFormat
	}
	if opts.MP4Mode > MP4ModeFragmented {
		return ErrInvalidData
	}
	if opts.FragmentDuration < 0 || opts.MaxFragmentBytes < 0 {
		return ErrInvalidData
	}
	if opts.Format != FormatMP4 && opts.MP4Mode != MP4ModeAuto {
		return ErrInvalidData
	}
	if opts.Format == FormatMP4 && opts.MP4Mode == MP4ModeProgressive {
		if _, ok := w.(seekableWriter); !ok {
			return ErrNotSeekable
		}
	}
	return nil
}
