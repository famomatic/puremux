// Package nal converts NAL-unit framing without decoding payloads.
package nal

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

func LengthPrefixedToAnnexB(data []byte, lengthSize int) ([]byte, error) {
	if lengthSize != 1 && lengthSize != 2 && lengthSize != 4 {
		return nil, errors.New("nal: length field must be 1, 2, or 4 bytes")
	}
	var output []byte
	for offset := 0; offset < len(data); {
		if len(data)-offset < lengthSize {
			return nil, io.ErrUnexpectedEOF
		}
		length := uint32(0)
		for _, value := range data[offset : offset+lengthSize] {
			length = length<<8 | uint32(value)
		}
		offset += lengthSize
		if length == 0 || uint64(length) > uint64(len(data)-offset) {
			return nil, errors.New("nal: invalid length-prefixed unit")
		}
		output = append(output, 0, 0, 0, 1)
		output = append(output, data[offset:offset+int(length)]...)
		offset += int(length)
	}
	return output, nil
}

func AnnexBToLengthPrefixed(data []byte, lengthSize int) ([]byte, error) {
	if lengthSize != 1 && lengthSize != 2 && lengthSize != 4 {
		return nil, errors.New("nal: length field must be 1, 2, or 4 bytes")
	}
	starts := startCodes(data)
	if len(starts) == 0 {
		return nil, errors.New("nal: Annex-B start code not found")
	}
	var output []byte
	for i, start := range starts {
		unitStart := start.offset + start.width
		unitEnd := len(data)
		if i+1 < len(starts) {
			unitEnd = starts[i+1].offset
		}
		for unitEnd > unitStart && data[unitEnd-1] == 0 {
			unitEnd--
		}
		length := unitEnd - unitStart
		if length <= 0 || uint64(length) >= uint64(1)<<(8*lengthSize) || length > math.MaxUint32 {
			return nil, errors.New("nal: unit does not fit configured length field")
		}
		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(length))
		output = append(output, encoded[4-lengthSize:]...)
		output = append(output, data[unitStart:unitEnd]...)
	}
	return output, nil
}

type startCode struct{ offset, width int }

func startCodes(data []byte) []startCode {
	var result []startCode
	for i := 0; i+3 <= len(data); {
		if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			result = append(result, startCode{i, 4})
			i += 4
		} else if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			result = append(result, startCode{i, 3})
			i += 3
		} else {
			i++
		}
	}
	return result
}
