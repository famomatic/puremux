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
	// VP9's uncompressed frame header is MSB-first bitpacked: the f(n) reader
	// (VP9 bitstream spec section 9.1 / libvpx vpx_rb_read_bit) consumes the
	// HIGH bit of each byte first. Field order when show_existing_frame == 0:
	//   frame_marker(2) | profile_low_bit(1) | profile_high_bit(1)
	//   [reserved_zero(1) if profile == 3] | show_existing_frame(1)
	//   | frame_type(1)   where 0 = KEY_FRAME
	// profile 3 inserts an extra reserved_zero bit before show_existing_frame,
	// shifting every subsequent field by one.
	bitOff := 0
	// frame_marker must be 0b10 for a valid VP9 frame.
	marker, _, ok := readBitsMSB(data, bitOff, 2)
	if !ok || marker != 0b10 {
		return false
	}
	bitOff += 2
	profLow, _, ok := readBitsMSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	profHigh, _, ok := readBitsMSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	profile := profLow | (profHigh << 1)
	// profile == 3 carries one extra reserved_zero bit before
	// show_existing_frame (VP9 spec section 9.2 "frame_header()").
	if profile == 3 {
		if _, _, ok := readBitsMSB(data, bitOff, 1); !ok {
			return false
		}
		bitOff += 1
	}
	sef, _, ok := readBitsMSB(data, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += 1
	if sef == 1 {
		// Refers to a previously-shown frame; not a new sync frame.
		return false
	}
	frameType, _, ok := readBitsMSB(data, bitOff, 1)
	if !ok {
		return false
	}
	return frameType == 0
}