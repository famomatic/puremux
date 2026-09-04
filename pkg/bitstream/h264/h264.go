// Package h264 handles AVCDecoderConfigurationRecord and NAL framing.
package h264

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/famomatic/puremux/pkg/bitstream/nal"
)

type Configuration struct {
	LengthSize int
	SPS        [][]byte
	PPS        [][]byte
	SPSExt     [][]byte
}

func ParseAVCC(data []byte) (Configuration, error) {
	if len(data) < 7 || data[0] != 1 || data[4]&0xfc != 0xfc || data[5]&0xe0 != 0xe0 {
		return Configuration{}, errors.New("h264: invalid AVC configuration record")
	}
	config := Configuration{LengthSize: int(data[4]&3) + 1}
	if config.LengthSize == 3 {
		return Configuration{}, errors.New("h264: reserved NAL length size")
	}
	offset := 6
	readUnits := func(count int, dst *[][]byte) error {
		for range count {
			if len(data)-offset < 2 {
				return io.ErrUnexpectedEOF
			}
			length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if length == 0 || length > len(data)-offset {
				return io.ErrUnexpectedEOF
			}
			*dst = append(*dst, append([]byte(nil), data[offset:offset+length]...))
			offset += length
		}
		return nil
	}
	if err := readUnits(int(data[5]&0x1f), &config.SPS); err != nil {
		return Configuration{}, err
	}
	if offset >= len(data) {
		return Configuration{}, io.ErrUnexpectedEOF
	}
	pps := int(data[offset])
	offset++
	if err := readUnits(pps, &config.PPS); err != nil {
		return Configuration{}, err
	}
	if avccHasProfileExtension(data[1]) {
		if len(data)-offset < 4 || data[offset]&0xfc != 0xfc ||
			data[offset+1]&0xf8 != 0xf8 || data[offset+2]&0xf8 != 0xf8 {
			return Configuration{}, errors.New("h264: invalid profile extension")
		}
		extensionCount := int(data[offset+3])
		offset += 4
		if err := readUnits(extensionCount, &config.SPSExt); err != nil {
			return Configuration{}, err
		}
	}
	if offset != len(data) {
		return Configuration{}, errors.New("h264: trailing configuration data")
	}
	return config, nil
}

func avccHasProfileExtension(profile byte) bool {
	switch profile {
	case 100, 110, 122, 144:
		return true
	default:
		return false
	}
}

// ValidateAVCC validates the configuration record and requires type-correct,
// non-empty SPS and PPS arrays.
func ValidateAVCC(data []byte) error {
	config, err := ParseAVCC(data)
	if err != nil || len(config.SPS) == 0 || len(config.PPS) == 0 {
		return errors.New("h264: invalid AVC configuration")
	}
	for _, unit := range config.SPS {
		if len(unit) == 0 || unit[0]&0x80 != 0 || unit[0]&0x1f != 7 {
			return errors.New("h264: invalid SPS")
		}
	}
	for _, unit := range config.PPS {
		if len(unit) == 0 || unit[0]&0x80 != 0 || unit[0]&0x1f != 8 {
			return errors.New("h264: invalid PPS")
		}
	}
	for _, unit := range config.SPSExt {
		if len(unit) == 0 || unit[0]&0x80 != 0 || unit[0]&0x1f != 13 {
			return errors.New("h264: invalid SPS extension")
		}
	}
	return nil
}

func AVCCToAnnexB(config Configuration, packet []byte, prependParameterSets bool) ([]byte, error) {
	output, err := nal.LengthPrefixedToAnnexB(packet, config.LengthSize)
	if err != nil {
		return nil, err
	}
	if !prependParameterSets {
		return output, nil
	}
	prefix := make([]byte, 0)
	for _, unit := range append(append([][]byte(nil), config.SPS...), config.PPS...) {
		if len(unit) == 0 {
			return nil, errors.New("h264: empty parameter set")
		}
		prefix = append(prefix, 0, 0, 0, 1)
		prefix = append(prefix, unit...)
	}
	return append(prefix, output...), nil
}

func AnnexBToAVCC(packet []byte, lengthSize int) ([]byte, error) {
	return nal.AnnexBToLengthPrefixed(packet, lengthSize)
}
