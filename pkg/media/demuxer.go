package media

import (
	"context"
	"errors"
)

// SeekFlags control the eligible indexed point and direction. With no flags,
// Seek chooses the earliest sync point at or after Target. SeekBackward
// chooses the latest eligible point at or before Target. SeekAny permits
// non-sync video points; audio points are always treated as sync points. A
// container may return an earlier timestamp when decoding preroll is needed.
type SeekFlags uint8

const (
	SeekBackward SeekFlags = 1 << iota
	SeekAny
)

const knownSeekFlags = SeekBackward | SeekAny

func validateSeekFlags(flags SeekFlags) error {
	if flags & ^knownSeekFlags != 0 {
		return errors.New("media: unknown seek flags")
	}
	return nil
}

func validateSeekRequest(req SeekRequest, streamCount int) error {
	if err := validateSeekFlags(req.Flags); err != nil {
		return err
	}
	if req.StreamIndex < -1 || req.StreamIndex >= streamCount {
		return errors.New("media: seek stream index out of range")
	}
	return nil
}

type SeekRequest struct {
	// StreamIndex selects the time base for Target. -1 means the demuxer's
	// global nanosecond time base (Rational{Num: 1, Den: 1e9}).
	StreamIndex int
	Target      int64
	Flags       SeekFlags
}

type SeekResult struct {
	StreamIndex int
	Timestamp   int64
}

// Demuxer yields compressed packets in container order. Implementations must
// make ReadPacket return promptly when ctx is canceled or Close is called.
// Seek invalidates internal queued packets; packets already handed to callers
// remain valid until their individual Release calls.
type Demuxer interface {
	Streams() []Stream
	Info() Info
	ReadPacket(context.Context) (*Packet, error)
	Seek(context.Context, SeekRequest) (SeekResult, error)
	Close() error
}

type OpenOptions struct {
	// FormatHint bypasses probing. It is required for non-seekable Sources.
	FormatHint Format
	// MaxProbeBytes caps bytes read during format detection. Zero uses the
	// implementation default; positive values below four are invalid.
	MaxProbeBytes int64
}
