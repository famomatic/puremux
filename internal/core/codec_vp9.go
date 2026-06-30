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
	// The keyframe flag is in the first frame's header, which always begins
	// at offset 0 regardless of whether a superframe index is appended.
	// VP9 uncompressed frame header: bit 0 of byte 0 = frame_marker LSB;
	// the keyframe bit (frame_type) follows profile bits. We read the
	// minimal header: byte0 low bits encode frame_marker (2 bits) then
	// profile_low (1 bit) then profile_high (1 bit) then show_existing_frame
	// (1 bit) then frame_type (1 bit) where 0 = KEY_FRAME.
	b0 := data[0]
	// frame_marker must be 0b10 for a valid VP9 frame.
	if b0&0x03 != 0x02 {
		return false
	}
	// bits: [1:0] frame_marker, [2] profile_low, [3] profile_high (if
	// profile_low==1 this is reserved), [4] show_existing_frame, [5] frame_type.
	// For the common single-profile case frame_type is bit 5.
	frameType := (b0 >> 5) & 0x01
	return frameType == 0
}