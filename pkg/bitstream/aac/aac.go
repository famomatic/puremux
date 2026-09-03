// Package aac converts MPEG-4 AudioSpecificConfig and ADTS framing without
// decoding AAC access units.
package aac

import (
	"errors"
	"io"
)

var sampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

type Config struct {
	AudioObjectType int
	SampleRate      int
	FrequencyIndex  int
	ChannelConfig   int
}

type ADTSHeader struct {
	MPEGVersion    int
	Profile        int
	SampleRate     int
	FrequencyIndex int
	ChannelConfig  int
	FrameLength    int
	HeaderLength   int
	Samples        int
}

func ParseASC(data []byte) (Config, error) {
	r := bitReader{data: data}
	objectType, ok := r.read(5)
	if !ok {
		return Config{}, io.ErrUnexpectedEOF
	}
	if objectType == 31 {
		extension, ok := r.read(6)
		if !ok {
			return Config{}, io.ErrUnexpectedEOF
		}
		objectType = 32 + extension
	}
	frequencyIndex, ok := r.read(4)
	if !ok {
		return Config{}, io.ErrUnexpectedEOF
	}
	sampleRate := 0
	if frequencyIndex == 15 {
		explicit, ok := r.read(24)
		if !ok {
			return Config{}, io.ErrUnexpectedEOF
		}
		sampleRate = explicit
	} else if frequencyIndex < len(sampleRates) {
		sampleRate = sampleRates[frequencyIndex]
	} else {
		return Config{}, errors.New("aac: reserved frequency index")
	}
	channels, ok := r.read(4)
	if !ok {
		return Config{}, io.ErrUnexpectedEOF
	}
	if objectType == 0 || sampleRate == 0 || channels == 0 {
		return Config{}, errors.New("aac: unsupported ASC object, rate, or program config")
	}
	return Config{AudioObjectType: objectType, SampleRate: sampleRate, FrequencyIndex: frequencyIndex, ChannelConfig: channels}, nil
}

func ConfigFromASC(data []byte) (Config, error) { return ParseASC(data) }

func ASC(config Config) ([]byte, error) {
	if config.AudioObjectType <= 0 || config.AudioObjectType >= 32 || config.ChannelConfig <= 0 || config.ChannelConfig > 15 {
		return nil, errors.New("aac: configuration does not fit compact ASC")
	}
	index := config.FrequencyIndex
	if index < 0 || index >= len(sampleRates) || sampleRates[index] != config.SampleRate {
		index = -1
		for i, rate := range sampleRates {
			if rate == config.SampleRate {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return nil, errors.New("aac: ADTS requires an indexed sample rate")
	}
	value := uint16(config.AudioObjectType)<<11 | uint16(index)<<7 | uint16(config.ChannelConfig)<<3
	return []byte{byte(value >> 8), byte(value)}, nil
}

func WrapADTS(config Config, raw []byte) ([]byte, error) {
	if len(raw) > 0x1fff-7 {
		return nil, errors.New("aac: access unit too large for ADTS")
	}
	if config.AudioObjectType < 1 || config.AudioObjectType > 4 || config.ChannelConfig < 1 || config.ChannelConfig > 7 {
		return nil, errors.New("aac: configuration unsupported by ADTS")
	}
	index := config.FrequencyIndex
	if index < 0 || index >= len(sampleRates) || sampleRates[index] != config.SampleRate {
		index = -1
		for i, rate := range sampleRates {
			if rate == config.SampleRate {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return nil, errors.New("aac: sample rate unavailable in ADTS")
	}
	length := len(raw) + 7
	header := make([]byte, 7, length)
	header[0] = 0xff
	header[1] = 0xf1 // MPEG-4, layer 0, protection_absent 1.
	header[2] = byte((config.AudioObjectType-1)<<6 | index<<2 | config.ChannelConfig>>2)
	header[3] = byte((config.ChannelConfig&3)<<6 | length>>11)
	header[4] = byte(length >> 3)
	header[5] = byte(length<<5) | 0x1f
	header[6] = 0xfc
	return append(header, raw...), nil
}

func ParseADTS(data []byte) (ADTSHeader, error) {
	if len(data) < 7 {
		return ADTSHeader{}, io.ErrUnexpectedEOF
	}
	if data[0] != 0xff || data[1]&0xf6 != 0xf0 {
		return ADTSHeader{}, errors.New("aac: invalid ADTS sync/layer")
	}
	protectionAbsent := data[1]&1 != 0
	headerLength := 7
	if !protectionAbsent {
		headerLength = 9
		if len(data) < headerLength {
			return ADTSHeader{}, io.ErrUnexpectedEOF
		}
	}
	index := int(data[2]>>2) & 0xf
	if index >= len(sampleRates) {
		return ADTSHeader{}, errors.New("aac: reserved ADTS frequency index")
	}
	channels := int(data[2]&1)<<2 | int(data[3]>>6)
	length := int(data[3]&3)<<11 | int(data[4])<<3 | int(data[5]>>5)
	if channels == 0 || length < headerLength || length > len(data) {
		return ADTSHeader{}, io.ErrUnexpectedEOF
	}
	blocks := int(data[6]&3) + 1
	return ADTSHeader{
		MPEGVersion: int(data[1] >> 3 & 1), Profile: int(data[2]>>6) + 1,
		SampleRate: sampleRates[index], FrequencyIndex: index, ChannelConfig: channels,
		FrameLength: length, HeaderLength: headerLength, Samples: 1024 * blocks,
	}, nil
}

func StripADTS(frame []byte) ([]byte, Config, error) {
	header, err := ParseADTS(frame)
	if err != nil {
		return nil, Config{}, err
	}
	config := Config{AudioObjectType: header.Profile, SampleRate: header.SampleRate, FrequencyIndex: header.FrequencyIndex, ChannelConfig: header.ChannelConfig}
	return append([]byte(nil), frame[header.HeaderLength:header.FrameLength]...), config, nil
}

type bitReader struct {
	data []byte
	bit  int
}

func (r *bitReader) read(count int) (int, bool) {
	if count < 0 || r.bit+count > len(r.data)*8 {
		return 0, false
	}
	value := 0
	for range count {
		value = value<<1 | int(r.data[r.bit/8]>>(7-uint(r.bit%8))&1)
		r.bit++
	}
	return value, true
}
