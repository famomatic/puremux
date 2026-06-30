package core

import "time"

// CodecType identifies the compressed codec carried by a Packet.
type CodecType uint8

const (
	CodecUnknown CodecType = iota
	CodecVP8
	CodecVP9
	CodecAV1
	CodecOpus
)

// String returns the canonical lowercase codec name used in WebM codec IDs.
func (c CodecType) String() string {
	switch c {
	case CodecVP8:
		return "vp8"
	case CodecVP9:
		return "vp9"
	case CodecAV1:
		return "av1"
	case CodecOpus:
		return "opus"
	default:
		return "unknown"
	}
}

// IsVideo reports whether the codec is a video track.
func (c CodecType) IsVideo() bool {
	switch c {
	case CodecVP8, CodecVP9, CodecAV1:
		return true
	default:
		return false
	}
}

// Packet is the telemetry primitive carrying an opaque compressed payload.
//
// The Data slice MUST be treated as opaque: callers may inspect codec packet
// headers via a CodecKeyframeDetector but MUST NOT decode the payload to
// pixels/PCM (see ARCHITECTURE.md section 4).
type Packet struct {
	Data       []byte
	PTS        time.Duration
	DTS        time.Duration
	IsKeyframe bool
	Codec      CodecType
	TrackID    int
}

// Reset clears the packet for reuse without releasing the Data backing array.
// Call before returning the packet to the pool.
func (p *Packet) Reset() {
	p.Data = p.Data[:0]
	p.PTS = 0
	p.DTS = 0
	p.IsKeyframe = false
	p.Codec = CodecUnknown
	p.TrackID = 0
}
