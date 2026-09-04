// Package media exposes compressed-media probing and demuxing primitives.
// It never decodes audio samples or video pixels.
package media

import (
	"errors"
	"math"
	"math/bits"
	"sync/atomic"
	"time"
)

var (
	ErrClosed            = errors.New("media: closed")
	ErrInvalidData       = errors.New("media: invalid data")
	ErrNotSeekable       = errors.New("media: input is not seekable")
	ErrSourceChanged     = errors.New("media: source changed during access")
	ErrUnsupportedFormat = errors.New("media: unsupported format")
	ErrUnsupportedCodec  = errors.New("media: unsupported codec")
	ErrIncompatible      = errors.New("media: incompatible input and output")
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
	v, ok := rescaleInteger(ticks, r.Num, int64(time.Second), r.Den, 1)
	return time.Duration(v), ok
}

// Rescale converts a value from r into dst without using floating point.
// Division truncates toward zero, matching Go duration conversion semantics.
func (r Rational) Rescale(value int64, dst Rational) (int64, bool) {
	if !r.Valid() || !dst.Valid() {
		return 0, false
	}
	return rescaleInteger(value, r.Num, dst.Den, r.Den, dst.Num)
}

// rescaleInteger computes value*n1*n2/(d1*d2) without heap allocation. The
// five signed inputs can span a 192-bit numerator and a 128-bit denominator;
// fixed-width long division keeps the result exact and truncates toward zero.
func rescaleInteger(value, n1, n2, d1, d2 int64) (int64, bool) {
	if d1 == 0 || d2 == 0 {
		return 0, false
	}
	negative := (value < 0) != (n1 < 0) != (n2 < 0) != (d1 < 0) != (d2 < 0)
	numerators := [3]uint64{unsignedMagnitude(value), unsignedMagnitude(n1), unsignedMagnitude(n2)}
	denominators := [2]uint64{unsignedMagnitude(d1), unsignedMagnitude(d2)}
	for i := range numerators {
		for j := range denominators {
			g := gcd64(numerators[i], denominators[j])
			if g > 1 {
				numerators[i] /= g
				denominators[j] /= g
			}
		}
	}

	numerator := mul192(numerators[0], numerators[1], numerators[2])
	denHi, denLo := bits.Mul64(denominators[0], denominators[1])
	quotient, overflow := div192By128(numerator, denHi, denLo)
	if overflow || (!negative && quotient > math.MaxInt64) || (negative && quotient > uint64(1)<<63) {
		return 0, false
	}
	if negative {
		if quotient == uint64(1)<<63 {
			return math.MinInt64, true
		}
		return -int64(quotient), true
	}
	return int64(quotient), true
}

func unsignedMagnitude(v int64) uint64 {
	if v >= 0 {
		return uint64(v)
	}
	return uint64(-(v + 1)) + 1
}

func gcd64(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// mul192 returns a*b*c as little-endian 64-bit limbs. The product of three
// uint64 values always fits in the three limbs.
func mul192(a, b, c uint64) [3]uint64 {
	hi, lo := bits.Mul64(a, b)
	upperHi, upperLo := bits.Mul64(hi, c)
	lowerHi, lowerLo := bits.Mul64(lo, c)
	middle, carry := bits.Add64(upperLo, lowerHi, 0)
	return [3]uint64{lowerLo, middle, upperHi + carry}
}

// div192By128 returns the low 64 quotient bits and reports whether any higher
// quotient bit was set. denHi:denLo must be non-zero.
func div192By128(n [3]uint64, denHi, denLo uint64) (uint64, bool) {
	if n[2] == 0 && denHi == 0 {
		if n[1] >= denLo {
			return 0, true
		}
		quotient, _ := bits.Div64(n[1], n[0], denLo)
		return quotient, false
	}
	var remHi, remLo uint64
	var quotient uint64
	var overflow bool
	for bit := 191; bit >= 0; bit-- {
		incoming := n[bit/64] >> uint(bit%64) & 1
		carry := remHi >> 63
		remHi = remHi<<1 | remLo>>63
		remLo = remLo<<1 | incoming
		if carry != 0 || remHi > denHi || (remHi == denHi && remLo >= denLo) {
			var borrow uint64
			remLo, borrow = bits.Sub64(remLo, denLo, 0)
			remHi, _ = bits.Sub64(remHi, denHi, borrow)
			if bit >= 64 {
				overflow = true
			} else {
				quotient |= uint64(1) << uint(bit)
			}
		}
	}
	return quotient, overflow
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
	released atomic.Bool
}

func (p *Packet) Keyframe() bool { return p != nil && p.Flags&PacketKeyframe != 0 }

func (p *Packet) Release() {
	if p == nil || !p.released.CompareAndSwap(false, true) {
		return
	}
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
	// CodecConfigVorbisHeaders is the three mandatory Vorbis header packets
	// encoded with Matroska/Xiph lacing.
	CodecConfigVorbisHeaders
	// CodecConfigVP9FeatureMetadata is the ID/length/value feature list used
	// as Matroska V_VP9 CodecPrivate. It is not an MP4 vpcC record.
	CodecConfigVP9FeatureMetadata
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
