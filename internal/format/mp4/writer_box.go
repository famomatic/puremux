package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/av1"
)

var (
	ErrInvalidOutputTrack  = errors.New("mp4: invalid output track")
	ErrInvalidOutputSample = errors.New("mp4: invalid output sample")
	ErrOutputTooLarge      = errors.New("mp4: output box or offset too large")
)

// OutputTrack is the exact container-facing description used by the MP4
// serializer. TimeScale is ticks per second and Config contains the payload of
// the codec configuration box (or ASC for AAC).
type OutputTrack struct {
	ID         int
	Codec      core.CodecType
	TimeScale  uint32
	Width      int
	Height     int
	Channels   int
	SampleRate int
	ConfigType string
	Config     []byte
	Language   string
}

// OutputSample is one MP4 sample. All timing fields use the owning track's
// time scale. Data is an MP4-native compressed sample and remains opaque.
type OutputSample struct {
	TrackID  int
	DTS      int64
	PTS      int64
	Duration int64
	Keyframe bool
	Data     []byte
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func outputBox(typ string, payload []byte) ([]byte, error) {
	if len(typ) != 4 || uint64(len(payload)) > math.MaxUint32-8 {
		return nil, ErrOutputTooLarge
	}
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b, nil
}

func outputFullBox(typ string, version byte, flags uint32, payload []byte) ([]byte, error) {
	b := make([]byte, 4+len(payload))
	b[0] = version
	b[1] = byte(flags >> 16)
	b[2] = byte(flags >> 8)
	b[3] = byte(flags)
	copy(b[4:], payload)
	return outputBox(typ, b)
}

func appendBox(dst *bytes.Buffer, typ string, payload []byte) error {
	b, err := outputBox(typ, payload)
	if err != nil {
		return err
	}
	_, err = dst.Write(b)
	return err
}

func putU16(b *bytes.Buffer, value uint16) { _ = binary.Write(b, binary.BigEndian, value) }
func putU32(b *bytes.Buffer, value uint32) { _ = binary.Write(b, binary.BigEndian, value) }
func putU64(b *bytes.Buffer, value uint64) { _ = binary.Write(b, binary.BigEndian, value) }
func putI32(b *bytes.Buffer, value int32)  { _ = binary.Write(b, binary.BigEndian, value) }
func putI64(b *bytes.Buffer, value int64)  { _ = binary.Write(b, binary.BigEndian, value) }

func checkedAddInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSubInt64(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		if a >= 0 {
			return 0, false
		}
		return a - b, true
	}
	return checkedAddInt64(a, -b)
}

func validateOutputTrack(t OutputTrack) error {
	if t.Language != "" {
		if len(t.Language) != 3 {
			return ErrInvalidOutputTrack
		}
		for _, c := range t.Language {
			if c < 'a' || c > 'z' {
				return ErrInvalidOutputTrack
			}
		}
	}
	if t.ID <= 0 || t.TimeScale == 0 || len(t.Config) == 0 {
		return ErrInvalidOutputTrack
	}
	if t.Codec.IsVideo() {
		if t.Width <= 0 || t.Width > math.MaxUint16 || t.Height <= 0 || t.Height > math.MaxUint16 {
			return ErrInvalidOutputTrack
		}
	} else if t.Codec.IsAudio() {
		if t.Channels <= 0 || t.Channels > math.MaxUint16 || t.SampleRate <= 0 || t.SampleRate > 65535 {
			return ErrInvalidOutputTrack
		}
	} else {
		return ErrInvalidOutputTrack
	}
	want := map[core.CodecType]string{
		core.CodecH264: "avcC", core.CodecHEVC: "hvcC", core.CodecAV1: "av1C",
		core.CodecVP9: "vpcC", core.CodecAAC: "asc", core.CodecOpus: "dOps",
		core.CodecFLAC: "dfLa",
	}[t.Codec]
	if want == "" || t.ConfigType != want {
		return ErrInvalidOutputTrack
	}
	return validateCodecConfig(t)
}

func sampleEntry(t OutputTrack) ([]byte, error) {
	if err := validateOutputTrack(t); err != nil {
		return nil, err
	}
	if t.Codec.IsVideo() {
		return visualSampleEntry(t)
	}
	return audioSampleEntry(t)
}

func visualSampleEntry(t OutputTrack) ([]byte, error) {
	entryType := map[core.CodecType]string{
		core.CodecH264: "avc1", core.CodecHEVC: "hvc1",
		core.CodecAV1: "av01", core.CodecVP9: "vp09",
	}[t.Codec]
	var body bytes.Buffer
	body.Write(make([]byte, 6))
	putU16(&body, 1)
	body.Write(make([]byte, 16))
	putU16(&body, uint16(t.Width))
	putU16(&body, uint16(t.Height))
	putU32(&body, 0x00480000)
	putU32(&body, 0x00480000)
	putU32(&body, 0)
	putU16(&body, 1)
	body.Write(make([]byte, 32))
	putU16(&body, 0x0018)
	putU16(&body, 0xffff)
	if err := appendBox(&body, t.ConfigType, t.Config); err != nil {
		return nil, err
	}
	if t.Codec == core.CodecAV1 {
		hasSequenceHeader, err := av1.HasSequenceHeader(t.Config)
		if err != nil {
			return nil, ErrInvalidOutputTrack
		}
		if !hasSequenceHeader {
			// AV1-ISOBMFF requires nclx when configOBUs has no Sequence
			// Header OBU. Values 2/2/2 mean unspecified CICP colour and the
			// final byte packs full_range_flag=0 plus seven reserved zeros.
			colr := []byte{'n', 'c', 'l', 'x', 0, 2, 0, 2, 0, 2, 0}
			if err := appendBox(&body, "colr", colr); err != nil {
				return nil, err
			}
		}
	}
	return outputBox(entryType, body.Bytes())
}

func audioSampleEntry(t OutputTrack) ([]byte, error) {
	entryType := map[core.CodecType]string{
		core.CodecAAC: "mp4a", core.CodecOpus: "Opus", core.CodecFLAC: "fLaC",
	}[t.Codec]
	var body bytes.Buffer
	body.Write(make([]byte, 6))
	putU16(&body, 1)
	body.Write(make([]byte, 8))
	putU16(&body, uint16(t.Channels))
	putU16(&body, 16)
	putU16(&body, 0)
	putU16(&body, 0)
	putU32(&body, uint32(t.SampleRate)<<16)
	if t.Codec == core.CodecAAC {
		esds, err := makeESDS(t.Config)
		if err != nil {
			return nil, err
		}
		body.Write(esds)
	} else if err := appendBox(&body, t.ConfigType, t.Config); err != nil {
		return nil, err
	}
	return outputBox(entryType, body.Bytes())
}

func descriptor(tag byte, payload []byte) ([]byte, error) {
	if len(payload) > 0x7f {
		return nil, ErrInvalidOutputTrack
	}
	b := make([]byte, 2+len(payload))
	b[0], b[1] = tag, byte(len(payload))
	copy(b[2:], payload)
	return b, nil
}

func makeESDS(asc []byte) ([]byte, error) {
	if len(asc) == 0 || len(asc) > 64 {
		return nil, ErrInvalidOutputTrack
	}
	dsi, _ := descriptor(0x05, asc)
	var dec bytes.Buffer
	dec.WriteByte(0x40) // MPEG-4 Audio objectTypeIndication
	dec.WriteByte(0x15) // streamType=5 audio, upstream=0, reserved=1
	dec.Write([]byte{0, 0, 0})
	putU32(&dec, 0)
	putU32(&dec, 0)
	dec.Write(dsi)
	decDesc, _ := descriptor(0x04, dec.Bytes())
	sl, _ := descriptor(0x06, []byte{0x02})
	var es bytes.Buffer
	putU16(&es, 1)
	es.WriteByte(0)
	es.Write(decDesc)
	es.Write(sl)
	esDesc, err := descriptor(0x03, es.Bytes())
	if err != nil {
		return nil, err
	}
	return outputFullBox("esds", 0, 0, esDesc)
}
