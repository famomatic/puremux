// Package opus converts and validates Opus identification/configuration
// headers without decoding audio samples.
package opus

import (
	"encoding/binary"
	"errors"
	"io"
)

type Config struct {
	Channels             int
	PreSkip              uint16
	InputSampleRate      uint32
	OutputGain           int16
	ChannelMappingFamily byte
	StreamCount          byte
	CoupledCount         byte
	ChannelMapping       []byte
}

func ParseHead(data []byte) (Config, error) {
	if len(data) < 19 {
		return Config{}, io.ErrUnexpectedEOF
	}
	if string(data[:8]) != "OpusHead" || data[8] != 1 || data[9] == 0 {
		return Config{}, errors.New("opus: invalid OpusHead")
	}
	c := Config{Channels: int(data[9]), PreSkip: binary.LittleEndian.Uint16(data[10:12]),
		InputSampleRate: binary.LittleEndian.Uint32(data[12:16]),
		OutputGain:      int16(binary.LittleEndian.Uint16(data[16:18])), ChannelMappingFamily: data[18]}
	if c.ChannelMappingFamily == 0 {
		if c.Channels > 2 || len(data) != 19 {
			return Config{}, errors.New("opus: invalid mapping family 0")
		}
		return c, nil
	}
	if len(data) != 21+c.Channels {
		return Config{}, io.ErrUnexpectedEOF
	}
	c.StreamCount, c.CoupledCount = data[19], data[20]
	c.ChannelMapping = append([]byte(nil), data[21:]...)
	if err := validateChannelMapping(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func DOPSFromHead(data []byte) ([]byte, error) {
	c, err := ParseHead(data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 11, 13+c.Channels)
	out[0], out[1] = 0, byte(c.Channels)
	binary.BigEndian.PutUint16(out[2:4], c.PreSkip)
	binary.BigEndian.PutUint32(out[4:8], c.InputSampleRate)
	binary.BigEndian.PutUint16(out[8:10], uint16(c.OutputGain))
	out[10] = c.ChannelMappingFamily
	if c.ChannelMappingFamily != 0 {
		out = append(out, c.StreamCount, c.CoupledCount)
		out = append(out, c.ChannelMapping...)
	}
	return out, nil
}

func ParseDOPS(data []byte) (Config, error) {
	if len(data) < 11 {
		return Config{}, io.ErrUnexpectedEOF
	}
	if data[0] != 0 || data[1] == 0 {
		return Config{}, errors.New("opus: invalid dOps")
	}
	c := Config{Channels: int(data[1]), PreSkip: binary.BigEndian.Uint16(data[2:4]),
		InputSampleRate: binary.BigEndian.Uint32(data[4:8]),
		OutputGain:      int16(binary.BigEndian.Uint16(data[8:10])), ChannelMappingFamily: data[10]}
	if c.ChannelMappingFamily == 0 {
		if c.Channels > 2 || len(data) != 11 {
			return Config{}, errors.New("opus: invalid dOps mapping family 0")
		}
		return c, nil
	}
	if len(data) != 13+c.Channels {
		return Config{}, io.ErrUnexpectedEOF
	}
	c.StreamCount, c.CoupledCount = data[11], data[12]
	c.ChannelMapping = append([]byte(nil), data[13:]...)
	if err := validateChannelMapping(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// HeadFromDOPS converts the big-endian ISO BMFF record to the
// little-endian RFC 7845 identification header without changing its fields.
func HeadFromDOPS(data []byte) ([]byte, error) {
	c, err := ParseDOPS(data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 19, 21+c.Channels)
	copy(out, "OpusHead")
	out[8], out[9] = 1, byte(c.Channels)
	binary.LittleEndian.PutUint16(out[10:12], c.PreSkip)
	binary.LittleEndian.PutUint32(out[12:16], c.InputSampleRate)
	binary.LittleEndian.PutUint16(out[16:18], uint16(c.OutputGain))
	out[18] = c.ChannelMappingFamily
	if c.ChannelMappingFamily != 0 {
		out = append(out, c.StreamCount, c.CoupledCount)
		out = append(out, c.ChannelMapping...)
	}
	return out, nil
}

func validateChannelMapping(c Config) error {
	if c.StreamCount == 0 || c.CoupledCount > c.StreamCount {
		return errors.New("opus: invalid channel mapping")
	}
	decodedChannels := int(c.StreamCount) + int(c.CoupledCount)
	if decodedChannels > 255 {
		return errors.New("opus: too many decoded channels")
	}
	for _, index := range c.ChannelMapping {
		if index != 255 && int(index) >= decodedChannels {
			return errors.New("opus: channel map index out of range")
		}
	}
	return nil
}
