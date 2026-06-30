package core

// vp8Detector reads the VP8 frame tag (RFC 6386 section 9.1).
//
// First 3 bytes are the frame tag, little-endian. Bit 0 of byte 0 is the
// frame type: 0 = keyframe (I-frame), 1 = interframe.
type vp8Detector struct{}

func (vp8Detector) IsKeyframe(data []byte) bool {
	if len(data) < 1 {
		return false
	}
	// frame_type occupies bit 0 of the first tag byte; 0 => keyframe.
	return data[0]&0x01 == 0
}