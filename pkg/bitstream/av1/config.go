// Package av1 validates AV1 codec configuration without decoding frames.
package av1

import (
	"errors"
	"io"
)

// ValidateConfig validates an AV1CodecConfigurationRecord. ISO/IEC 14496-15
// packs the fixed fields MSB-first. configOBUs use AV1 low-overhead framing,
// where every OBU has a size field encoded as unsigned LEB128.
func ValidateConfig(data []byte) error {
	_, err := inspectConfig(data)
	return err
}

// HasSequenceHeader reports whether a valid AV1CodecConfigurationRecord has a
// Sequence Header OBU in configOBUs.
func HasSequenceHeader(data []byte) (bool, error) {
	return inspectConfig(data)
}

func inspectConfig(data []byte) (bool, error) {
	invalid := errors.New("av1: invalid AV1 configuration record")
	if len(data) < 4 || data[0] != 0x81 || data[3]&0xe0 != 0 ||
		data[1]>>5 > 2 || data[1]&0x1f > 23 {
		return false, invalid
	}
	// When initial_presentation_delay_present is zero, its four-bit field is
	// reserved and must also be zero.
	if data[3]&0x10 == 0 && data[3]&0x0f != 0 {
		return false, invalid
	}

	offset := 4
	obuIndex := 0
	hasSequenceHeader := false
	for offset < len(data) {
		header := data[offset]
		// obu_forbidden_bit and obu_reserved_1bit must be zero; configOBUs
		// always require obu_has_size_field=1.
		if header&0x81 != 0 || header&0x02 == 0 {
			return false, invalid
		}
		obuType := header >> 3 & 0x0f
		// Values 0 and 9..14 are reserved by the AV1 specification.
		if obuType == 0 || (obuType >= 9 && obuType <= 14) {
			return false, invalid
		}
		offset++
		if header&0x04 != 0 {
			if offset >= len(data) {
				return false, io.ErrUnexpectedEOF
			}
			// The low three bits of obu_extension_header are reserved.
			if data[offset]&0x07 != 0 {
				return false, invalid
			}
			offset++
		}
		size, used, ok := readLEB128(data[offset:])
		if !ok {
			return false, io.ErrUnexpectedEOF
		}
		offset += used
		if size > uint64(len(data)-offset) {
			return false, io.ErrUnexpectedEOF
		}
		if obuType == 1 {
			if hasSequenceHeader || obuIndex != 0 || size == 0 {
				return false, invalid
			}
			hasSequenceHeader = true
		}
		offset += int(size)
		obuIndex++
	}
	return hasSequenceHeader, nil
}

func readLEB128(data []byte) (uint64, int, bool) {
	var value uint64
	for i := 0; i < 8 && i < len(data); i++ {
		current := data[i]
		value |= uint64(current&0x7f) << (7 * i)
		if current&0x80 == 0 {
			return value, i + 1, true
		}
	}
	return 0, 0, false
}
