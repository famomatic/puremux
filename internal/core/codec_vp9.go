package core

// vp9Detector handles the VP9 frame header plus the optional superframe
// index appended at the tail of a packet (VP9 bitstream spec, "superframe"
// packing in WebRTC).
//
// A VP9 superframe index byte (if present) is the last byte of the packet:
//   - bits[7:5] == 0b110  (marker)
//   - bits[4:0] = number of frames - 1 (1..8 frames)
//   - bytes[1 .. 1+sz]   = frame sizes, where sz = 1 << (byte & 0x03)
// When present, the actual frame headers start at offset 0 and the index
// describes the sub-frame sizes. The keyframe flag lives in the first frame
// tag at byte offset 0, bit 0: 0 = keyframe.
type vp9Detector struct{}

func (vp9Detector) IsKeyframe(data []byte) bool {
	if len(data) < 1 {
		return false
	}
	// VP9 uncompressed frame header is LSB-first bitpacked (the spec's f(n)
	// reader consumes the low bit of each byte first). Field order when
	// show_existing_frame == 0:
	//   frame_marker(2) | profile_low_bit(1) | profile_high_bit(1)
	//   [reserved_zero(1) if profile == 3] | show_existing_frame(1)
	//   | frame_type(1)   where 0 = KEY_FRAME
	// The old code read frame_type at a fixed bit offset (5), which is only
	// correct for profile < 3; profile 3 inserts a reserved bit that shifts
	// every subsequent field by one.
	bitOff := 0
	// frame_marker must be 0b10 for a valid VP9 frame.
	marker, _, ok := readBitsLSB(data, bitOff, 2)
	if !ok || marker != 0b10 {
		return false
	}
	bitOff += 2
	profLow, _, ok := readBitsLSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	profHigh, _, ok := readBitsLSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	profile := profLow | (profHigh << 1)
	// profile == 3 carries one extra reserved_zero bit before
	// show_existing_frame (VP9 spec section 9.2 "frame_header()").
	if profile == 3 {
		_, _, ok := readBitsLSB(data, bitOff, 1)
		if !ok {
			return false
		}
		bitOff += 1
	}
	sef, _, ok := readBitsLSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	if sef == 1 {
		// Refers to a previously-shown frame; not a new sync frame.
		return false
	}
	frameType, _, ok := readBitsLSB(data, bitOff, 1)
	if !ok {
		return false
	}
	return frameType == 0
}

// readBitsLSB reads width bits from data starting at bitOff using LSB-first
// bitpacking (the VP9 f(n) reader consumes the low bit of each byte first).
// bit 0 of bitOff refers to the least significant bit of byte 0. Returns
// value, bits consumed, ok.
func readBitsLSB(data []byte, bitOff, width int) (val, bits int, ok bool) {
	if width <= 0 || width > 16 {
		return 0, 0, false
	}
	var acc uint32
	for i := 0; i < width; i++ {
		absBit := bitOff + i
		byteIdx := absBit / 8
		if byteIdx >= len(data) {
			return 0, 0, false
		}
		bit := (data[byteIdx] >> uint(absBit%8)) & 0x01
		acc |= uint32(bit) << uint(i)
	}
	return int(acc), width, true
}