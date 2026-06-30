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
	// Audio-only codecs supported by the WebM/Matroska containers. These
	// carry no keyframe concept and use the no-op detector (ARCHITECTURE.md
	// section 4). Vorbis is the legacy WebM audio codec; FLAC is a Matroska
	// (MKV) lossless codec not permitted in WebM.
	CodecVorbis
	CodecFLAC
	// CodecAAC is read-only: MP4 input may carry AAC audio. puremux never
	// writes AAC (no MP4 writer; AAC is not a WebM/MKV-pure codec we mux).
	CodecAAC
	// MPEG video codecs. Muxing these is patent-free (muxers do not decode or
	// implement the codec); they are carried opaque and read/written across
	// MKV (and MP4 input). VP8-style NAL keyframe detection applies.
	CodecH264 // H.264/AVC (V_MPEG4/ISO/AVC)
	CodecHEVC // H.265/HEVC (V_MPEGH/ISO/SHEVC)
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
	case CodecVorbis:
		return "vorbis"
	case CodecFLAC:
		return "flac"
	case CodecAAC:
		return "aac"
	case CodecH264:
		return "h264"
	case CodecHEVC:
		return "hevc"
	default:
		return "unknown"
	}
}

// IsVideo reports whether the codec is a video track.
func (c CodecType) IsVideo() bool {
	switch c {
	case CodecVP8, CodecVP9, CodecAV1, CodecH264, CodecHEVC:
		return true
	default:
		return false
	}
}

// IsAudio reports whether the codec is an audio track.
func (c CodecType) IsAudio() bool {
	switch c {
	case CodecOpus, CodecVorbis, CodecFLAC:
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
