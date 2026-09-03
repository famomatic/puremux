package media

import "context"

// SeekFlags describe whether a seek target must be a sync point and which
// direction is acceptable when the exact target is unavailable.
type SeekFlags uint8

const (
	SeekBackward SeekFlags = 1 << iota
	SeekAny
)

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
	FormatHint    Format
	MaxProbeBytes int64
}
