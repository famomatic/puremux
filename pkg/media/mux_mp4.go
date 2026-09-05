package media

import (
	"context"
	"fmt"
	"io"

	"github.com/famomatic/puremux/internal/core"
	formatmp4 "github.com/famomatic/puremux/internal/format/mp4"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
	"github.com/famomatic/puremux/pkg/bitstream/opus"
	"github.com/famomatic/puremux/pkg/bitstream/vp9"
)

type mp4SampleWriter interface {
	AddTrack(formatmp4.OutputTrack) (int, error)
	WriteSample(formatmp4.OutputSample) error
	Close() error
}

type mp4Muxer struct {
	allowMetadataLoss bool
	w                 mp4SampleWriter
	streams           []Stream
	trackByStream     []int
	started           bool
	closed            bool
	closeErr          error
}

func newMP4Muxer(dst io.Writer, opts MuxOptions) (Muxer, error) {
	mode := opts.MP4Mode
	if mode == MP4ModeAuto {
		if _, ok := dst.(seekableWriter); ok {
			mode = MP4ModeProgressive
		} else {
			mode = MP4ModeFragmented
		}
	}
	var writer mp4SampleWriter
	var err error
	if mode == MP4ModeProgressive {
		writer, err = formatmp4.NewProgressiveWriter(dst.(seekableWriter))
	} else {
		writer, err = formatmp4.NewFragmentedWriter(dst, opts.FragmentDuration, opts.MaxFragmentBytes)
	}
	if err != nil {
		return nil, err
	}
	return &mp4Muxer{w: writer, allowMetadataLoss: opts.AllowMetadataLoss}, nil
}

func (m *mp4Muxer) AddStream(stream Stream) (int, error) {
	if m.closed || m.started {
		return 0, errorsForMuxState(m.closed)
	}
	if !stream.TimeBase.Valid() || stream.TimeBase.Num <= 0 || stream.TimeBase.Den%stream.TimeBase.Num != 0 {
		return 0, fmt.Errorf("%w: MP4 requires an integral ticks-per-second time base", ErrInvalidData)
	}
	if !m.allowMetadataLoss {
		if err := validateOutputMetadata(stream, FormatMP4); err != nil {
			return 0, err
		}
	}
	codec := coreCodec(stream.Codec)
	if codec == core.CodecUnknown ||
		(codec.IsVideo() && stream.Type != MediaVideo) ||
		(codec.IsAudio() && stream.Type != MediaAudio) {
		return 0, fmt.Errorf("%w: stream type does not match codec", ErrInvalidData)
	}
	scale := stream.TimeBase.Den / stream.TimeBase.Num
	if scale <= 0 || scale > int64(^uint32(0)) {
		return 0, fmt.Errorf("%w: MP4 timescale", ErrInvalidData)
	}
	configType, config, err := normalizeMP4Config(stream)
	if err != nil {
		return 0, err
	}
	id := len(m.streams) + 1
	track := formatmp4.OutputTrack{ID: id, Codec: codec, TimeScale: uint32(scale),
		Width: stream.Width, Height: stream.Height, Channels: stream.Channels,
		SampleRate: stream.SampleRate, ConfigType: configType, Config: config, Language: stream.Language}
	if _, err := m.w.AddTrack(track); err != nil {
		return 0, err
	}
	copyStream := cloneStreams([]Stream{stream})[0]
	copyStream.Index = id - 1
	m.streams = append(m.streams, copyStream)
	m.trackByStream = append(m.trackByStream, id)
	return id - 1, nil
}

func normalizeMP4Config(stream Stream) (string, []byte, error) {
	data := append([]byte(nil), stream.Config.Data...)
	switch stream.Codec {
	case CodecH264:
		if stream.Config.Format != CodecConfigAVCC {
			return "", nil, ErrUnsupportedCodec
		}
		return "avcC", data, nil
	case CodecHEVC:
		if stream.Config.Format != CodecConfigHVCC {
			return "", nil, ErrUnsupportedCodec
		}
		return "hvcC", data, nil
	case CodecAV1:
		if stream.Config.Format != CodecConfigAV1C {
			return "", nil, ErrUnsupportedCodec
		}
		return "av1C", data, nil
	case CodecVP9:
		switch stream.Config.Format {
		case CodecConfigVPCC:
			return "vpcC", data, nil
		case CodecConfigVP9FeatureMetadata:
			converted, err := vp9.VPCCFromFeatureMetadata(data)
			return "vpcC", converted, err
		default:
			return "", nil, ErrUnsupportedCodec
		}
	case CodecAAC:
		if stream.Config.Format != CodecConfigASC {
			return "", nil, ErrUnsupportedCodec
		}
		return "asc", data, nil
	case CodecOpus:
		switch stream.Config.Format {
		case CodecConfigDOPS:
			return "dOps", data, nil
		case CodecConfigOpusHead:
			converted, err := opus.DOPSFromHead(data)
			return "dOps", converted, err
		default:
			return "", nil, ErrUnsupportedCodec
		}
	case CodecFLAC:
		switch stream.Config.Format {
		case CodecConfigFLACStreamInfo:
			if len(data) == 34 {
				converted, err := flac.DFLAPayload(data)
				return "dfLa", converted, err
			}
			if len(data) >= 4 && string(data[:4]) == "fLaC" {
				converted, err := flac.DFLAFromMatroskaCodecPrivate(data)
				return "dfLa", converted, err
			}
			// MP4 demuxers retain the complete dfLa FullBox payload. Validate
			// it here so an arbitrary 42-byte record is not passed onward.
			if _, err := flac.StreamInfoFromDFLA(data); err != nil {
				return "dfLa", nil, err
			}
			return "dfLa", data, nil
		default:
			return "", nil, ErrUnsupportedCodec
		}
	default:
		return "", nil, ErrUnsupportedCodec
	}
}

func (m *mp4Muxer) WritePacket(ctx context.Context, packet *Packet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrClosed
	}
	if packet == nil {
		return nil
	}
	if packet.StreamIndex < 0 || packet.StreamIndex >= len(m.streams) ||
		!packet.DTS.Valid || !packet.PTS.Valid || !packet.Duration.Valid {
		return ErrInvalidData
	}
	if packet.DiscardPadding != 0 {
		return fmt.Errorf("%w: MP4 discard padding is not representable by this writer", ErrUnsupportedFormat)
	}
	m.started = true
	return m.w.WriteSample(formatmp4.OutputSample{TrackID: m.trackByStream[packet.StreamIndex],
		DTS: packet.DTS.Value, PTS: packet.PTS.Value, Duration: packet.Duration.Value,
		Keyframe: packet.Keyframe(), Data: packet.Data})
}

func (m *mp4Muxer) Close() error {
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	m.closeErr = m.w.Close()
	return m.closeErr
}

func errorsForMuxState(closed bool) error {
	if closed {
		return ErrClosed
	}
	return errorsNewStreamsLocked
}

var errorsNewStreamsLocked = fmt.Errorf("media: streams are locked after the first packet")

func coreCodec(codec CodecID) core.CodecType {
	switch codec {
	case CodecVP8:
		return core.CodecVP8
	case CodecVP9:
		return core.CodecVP9
	case CodecAV1:
		return core.CodecAV1
	case CodecOpus:
		return core.CodecOpus
	case CodecVorbis:
		return core.CodecVorbis
	case CodecFLAC:
		return core.CodecFLAC
	case CodecAAC:
		return core.CodecAAC
	case CodecMP3:
		return core.CodecMP3
	case CodecH264:
		return core.CodecH264
	case CodecHEVC:
		return core.CodecHEVC
	default:
		return core.CodecUnknown
	}
}
