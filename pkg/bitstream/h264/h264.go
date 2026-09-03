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
}

func ParseAVCC(data []byte) (Configuration, error) {
	if len(data) < 7 || data[0] != 1 {
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
	return config, nil
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
