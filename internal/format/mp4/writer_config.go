package mp4

import (
	"errors"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/av1"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
	"github.com/famomatic/puremux/pkg/bitstream/h264"
	"github.com/famomatic/puremux/pkg/bitstream/hevc"
	"github.com/famomatic/puremux/pkg/bitstream/opus"
	"github.com/famomatic/puremux/pkg/bitstream/vp9"
)

func validateCodecConfig(t OutputTrack) error {
	switch t.Codec {
	case core.CodecH264:
		if err := h264.ValidateAVCC(t.Config); err != nil {
			return ErrInvalidOutputTrack
		}
	case core.CodecHEVC:
		if err := hevc.ValidateHVCC(t.Config); err != nil {
			return ErrInvalidOutputTrack
		}
	case core.CodecAAC:
		config, err := aac.ParseASC(t.Config)
		if err != nil || config.SampleRate != t.SampleRate || config.ChannelConfig != t.Channels {
			return ErrInvalidOutputTrack
		}
	case core.CodecOpus:
		config, err := opus.ParseDOPS(t.Config)
		if err != nil || config.Channels != t.Channels || t.SampleRate != 48000 {
			return ErrInvalidOutputTrack
		}
	case core.CodecFLAC:
		streamInfo, err := flac.StreamInfoFromDFLA(t.Config)
		if err != nil {
			return ErrInvalidOutputTrack
		}
		info, _ := flac.ParseStreamInfo(streamInfo)
		if info.SampleRate != t.SampleRate || info.Channels != t.Channels {
			return ErrInvalidOutputTrack
		}
	case core.CodecAV1:
		if err := av1.ValidateConfig(t.Config); err != nil {
			return ErrInvalidOutputTrack
		}
	case core.CodecVP9:
		if err := vp9.ValidateVPCC(t.Config); err != nil {
			return ErrInvalidOutputTrack
		}
	default:
		return errors.New("mp4: unsupported codec configuration")
	}
	return nil
}
