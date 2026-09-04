// Package vorbis validates Vorbis codec headers without decoding audio.
package vorbis

import (
	"encoding/binary"
	"errors"
)

// ValidateCodecPrivate validates the three mandatory Vorbis header packets in
// Matroska/Xiph lacing and checks the identification header against the track.
func ValidateCodecPrivate(data []byte, channels, sampleRate int) error {
	headers, ok := splitXiphLacedHeaders(data)
	if !ok {
		return errors.New("vorbis: invalid Xiph lacing")
	}
	identification := headers[0]
	if len(identification) != 30 || identification[0] != 1 || string(identification[1:7]) != "vorbis" ||
		binary.LittleEndian.Uint32(identification[7:11]) != 0 || int(identification[11]) != channels ||
		int(binary.LittleEndian.Uint32(identification[12:16])) != sampleRate || identification[29] != 1 {
		return errors.New("vorbis: invalid identification header")
	}
	shortBlock := identification[28] & 0x0f
	longBlock := identification[28] >> 4
	if shortBlock < 6 || longBlock > 13 || shortBlock > longBlock {
		return errors.New("vorbis: invalid block sizes")
	}
	if !validCommentHeader(headers[1]) {
		return errors.New("vorbis: invalid comment header")
	}
	if len(headers[2]) <= 7 || headers[2][0] != 5 || string(headers[2][1:7]) != "vorbis" {
		return errors.New("vorbis: invalid setup header")
	}
	return nil
}

func splitXiphLacedHeaders(data []byte) ([3][]byte, bool) {
	var headers [3][]byte
	if len(data) == 0 || data[0] != 2 {
		return headers, false
	}
	offset := 1
	var lengths [2]int
	for i := range lengths {
		for {
			if offset >= len(data) {
				return headers, false
			}
			value := int(data[offset])
			offset++
			if lengths[i] > len(data)-value {
				return headers, false
			}
			lengths[i] += value
			if value != 255 {
				break
			}
		}
	}
	if lengths[0] == 0 || lengths[1] == 0 || lengths[0] > len(data)-offset {
		return headers, false
	}
	headers[0] = data[offset : offset+lengths[0]]
	offset += lengths[0]
	if lengths[1] > len(data)-offset {
		return headers, false
	}
	headers[1] = data[offset : offset+lengths[1]]
	offset += lengths[1]
	headers[2] = data[offset:]
	return headers, len(headers[2]) > 0
}

func validCommentHeader(header []byte) bool {
	if len(header) < 16 || header[0] != 3 || string(header[1:7]) != "vorbis" {
		return false
	}
	offset := 7
	vendorLength := uint64(binary.LittleEndian.Uint32(header[offset : offset+4]))
	offset += 4
	if vendorLength > uint64(len(header)-offset) {
		return false
	}
	offset += int(vendorLength)
	if len(header)-offset < 4 {
		return false
	}
	commentCount := uint64(binary.LittleEndian.Uint32(header[offset : offset+4]))
	offset += 4
	for ; commentCount > 0; commentCount-- {
		if len(header)-offset < 4 {
			return false
		}
		length := uint64(binary.LittleEndian.Uint32(header[offset : offset+4]))
		offset += 4
		if length > uint64(len(header)-offset) {
			return false
		}
		offset += int(length)
	}
	return len(header)-offset == 1 && header[offset] == 1
}
