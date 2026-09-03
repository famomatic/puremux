// Package hevc handles HEVCDecoderConfigurationRecord and NAL framing.
package hevc

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/famomatic/puremux/pkg/bitstream/nal"
)

type Array struct {
	NALType uint8
	Units   [][]byte
}

type Configuration struct {
	LengthSize int
	Arrays     []Array
}

func ParseHVCC(data []byte) (Configuration, error) {
	if len(data) < 23 || data[0] != 1 {
		return Configuration{}, errors.New("hevc: invalid HEVC configuration record")
	}
	config := Configuration{LengthSize: int(data[21]&3) + 1}
	if config.LengthSize == 3 {
		return Configuration{}, errors.New("hevc: reserved NAL length size")
	}
	offset := 23
	for range int(data[22]) {
		if len(data)-offset < 3 {
			return Configuration{}, io.ErrUnexpectedEOF
		}
		array := Array{NALType: data[offset] & 0x3f}
		count := int(binary.BigEndian.Uint16(data[offset+1 : offset+3]))
		offset += 3
		for range count {
			if len(data)-offset < 2 {
				return Configuration{}, io.ErrUnexpectedEOF
			}
			length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if length == 0 || length > len(data)-offset {
				return Configuration{}, io.ErrUnexpectedEOF
			}
			array.Units = append(array.Units, append([]byte(nil), data[offset:offset+length]...))
			offset += length
		}
		config.Arrays = append(config.Arrays, array)
	}
	return config, nil
}

func HVCCToAnnexB(config Configuration, packet []byte, prependParameterSets bool) ([]byte, error) {
	output, err := nal.LengthPrefixedToAnnexB(packet, config.LengthSize)
	if err != nil {
		return nil, err
	}
	if !prependParameterSets {
		return output, nil
	}
	var prefix []byte
	for _, array := range config.Arrays {
		if array.NALType != 32 && array.NALType != 33 && array.NALType != 34 {
			continue
		}
		for _, unit := range array.Units {
			if len(unit) == 0 {
				return nil, errors.New("hevc: empty parameter set")
			}
			prefix = append(prefix, 0, 0, 0, 1)
			prefix = append(prefix, unit...)
		}
	}
	return append(prefix, output...), nil
}

func AnnexBToHVCC(packet []byte, lengthSize int) ([]byte, error) {
	return nal.AnnexBToLengthPrefixed(packet, lengthSize)
}
