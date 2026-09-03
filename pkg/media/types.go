// Package media exposes compressed-media probing and demuxing primitives.
// It never decodes audio samples or video pixels.
package media

import (
	"errors"
	"math"
	"math/big"
	"time"
)

var (
	ErrClosed            = errors.New("media: closed")
	ErrInvalidData       = errors.New("media: invalid data")
	ErrNotSeekable       = errors.New("media: input is not seekable")
	ErrSourceChanged     = errors.New("media: source changed during access")
	ErrUnsupportedFormat = errors.New("media: unsupported format")
	ErrUnsupportedCodec  = errors.New("media: unsupported codec")
)

// Format identifies either a byte container or a manifest format.
type Format uint8

const (
	FormatUnknown Format = iota
	FormatWebM
	FormatMatroska
	FormatMP4
	FormatOgg
	FormatMPEGTS
	FormatMP3
	FormatADTS
	FormatFLAC
	FormatHLS
	FormatDASH
)

func (f Format) String() string {
	switch f {
	case FormatWebM:
		return "webm"
	case FormatMatroska:
		return "matroska"
	case FormatMP4:
		return "mp4"
	case FormatOgg:
		return "ogg"
	case FormatMPEGTS:
		return "mpegts"
	case FormatMP3:
		return "mp3"
	case FormatADTS:
		return "adts"
	case FormatFLAC:
		return "flac"
	case FormatHLS:
		return "hls"
	case FormatDASH:
		return "dash"
	default:
		return "unknown"
	}
}

type MediaType uint8

const (
	MediaUnknown MediaType = iota
	MediaVideo
	MediaAudio
	MediaSubtitle
	MediaData
)

type CodecID uint16

const (
	CodecUnknown CodecID = iota
	CodecVP8
	CodecVP9
	CodecAV1
	CodecOpus
	CodecVorbis
	CodecFLAC
	CodecAAC
	CodecMP3
	CodecH264
	CodecHEVC
)

func (c CodecID) String() string {
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
	case CodecMP3:
		return "mp3"
	case CodecH264:
		return "h264"
	case CodecHEVC:
		return "hevc"
	default:
		return "unknown"
	}
}

// Rational is an exact time base or rate. Den must be positive for Valid to
// report true; Num may be negative for general rescaling operations.
type Rational struct {
	Num int64
	Den int64
}

func (r Rational) Valid() bool { return r.Den > 0 && r.Num != 0 }

// Duration converts ticks in this time base to a Go duration. It is intended
// as a boundary convenience, not as the canonical per-packet representation.
// The bool is false for an invalid time base or an overflowing result.
func (r Rational) Duration(ticks int64) (time.Duration, bool) {
	if !r.Valid() {
		return 0, false
	}
	n := new(big.Int).SetInt64(ticks)
	n.Mul(n, big.NewInt(r.Num))
	n.Mul(n, big.NewInt(int64(time.Second)))
	n.Quo(n, big.NewInt(r.Den))
	if !n.IsInt64() {
		return 0, false
	}
	v := n.Int64()
	if v == math.MinInt64 {
		return time.Duration(v), true
	}
	return time.Duration(v), true
}

// Rescale converts a value from r into dst without using floating point.
// Division truncates toward zero, matching Go duration conversion semantics.
func (r Rational) Rescale(value int64, dst Rational) (int64, bool) {
	if !r.Valid() || !dst.Valid() {
		return 0, false
	}
	n := new(big.Int).SetInt64(value)
	n.Mul(n, big.NewInt(r.Num))
	n.Mul(n, big.NewInt(dst.Den))
	n.Quo(n, big.NewInt(r.Den))
	n.Quo(n, big.NewInt(dst.Num))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// Timestamp distinguishes a legitimate zero timestamp from a missing value.
// Value is expressed in the owning Stream's TimeBase.
type Timestamp struct {
	Value int64
	Valid bool
}

func KnownTimestamp(value int64) Timestamp { return Timestamp{Value: value, Valid: true} }

func (t Timestamp) Duration(tb Rational) (time.Duration, bool) {
	if !t.Valid {
		return 0, false
	}
	return tb.Duration(t.Value)
}

type PacketFlags uint16

const (
	PacketKeyframe PacketFlags = 1 << iota
	PacketCorrupt
	PacketDiscontinuity
)

// Packet carries one compressed packet. Data remains valid until Release is
// called. Release is idempotent; callers must not retain or mutate Data after
// releasing a packet.
type Packet struct {
	StreamIndex int
	Data        []byte
	PTS         Timestamp
	DTS         Timestamp
	Duration    Timestamp
	Flags       PacketFlags
	Pos         int64
	// DiscardPadding applies to the end of this compressed packet. A negative
	// value denotes padding at the start, as defined by Matroska.
	DiscardPadding time.Duration

	release  func([]byte)
	released bool
}

func (p *Packet) Keyframe() bool { return p != nil && p.Flags&PacketKeyframe != 0 }

func (p *Packet) Release() {
	if p == nil || p.released {
		return
	}
	p.released = true
	data := p.Data
	p.Data = nil
	if p.release != nil {
		p.release(data)
	}
}

// NewPacket constructs a caller-owned packet. Release clears Data but does
// not pool caller memory. Demuxers use the same contract with an internal
// release callback when pooling is beneficial.
func NewPacket(streamIndex int, data []byte) *Packet {
	return &Packet{StreamIndex: streamIndex, Data: data}
}

type CodecConfigFormat uint8

const (
	CodecConfigUnknown CodecConfigFormat = iota
	CodecConfigOpusHead
	CodecConfigAVCC
	CodecConfigHVCC
	CodecConfigASC
	CodecConfigFLACStreamInfo
	CodecConfigDOPS
	CodecConfigAV1C
	CodecConfigVPCC
)

type CodecConfig struct {
	Format CodecConfigFormat
	Data   []byte
}

type Disposition uint32

const (
	DispositionDefault Disposition = 1 << iota
	DispositionForced
	DispositionHearingImpaired
	DispositionVisualImpaired
)

type Stream struct {
	Index       int
	ID          int64
	Type        MediaType
	Codec       CodecID
	TimeBase    Rational
	StartTime   Timestamp
	Duration    Timestamp
	BitRate     int64
	Disposition Disposition
	Language    string
	Metadata    map[string]string
	Config      CodecConfig

	SampleRate int
	Channels   int
	Width      int
	Height     int
	FrameRate  Rational

	CodecDelay    time.Duration
	SeekPreRoll   time.Duration
	DefaultPacket time.Duration
}

type Info struct {
	Format        Format
	FormatName    string
	Duration      time.Duration
	DurationKnown bool
	StartTime     time.Duration
	StartKnown    bool
	BitRate       int64
	Metadata      map[string]string
}
