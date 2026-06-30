package core

import "testing"

// H.264 NAL header byte construction (ITU-T H.264 section 7.3.1):
//   forbidden_zero_bit(1) | nal_ref_idc(2) | nal_unit_type(5)
//
// IDR slice (nal_unit_type=5, nal_ref_idc=3): 0 11 00101 = 0x65
// Non-IDR slice (nal_unit_type=1, nal_ref_idc=2): 0 10 00001 = 0x41
// SPS (nal_unit_type=7, nal_ref_idc=3): 0 11 00111 = 0x67

func h264AVCCPayload(nalUnits [][]byte) []byte {
	// 4-byte big-endian length prefix per NAL.
	out := []byte{}
	for _, n := range nalUnits {
		l := uint32(len(n))
		out = append(out, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
		out = append(out, n...)
	}
	return out
}

func h264AnnexBPayload(nalUnits [][]byte) []byte {
	out := []byte{}
	for _, n := range nalUnits {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, n...)
	}
	return out
}

func TestH264KeyframeAVCC(t *testing.T) {
	// IDR NAL (0x65) under a 4-byte length prefix -> keyframe.
	p := h264AVCCPayload([][]byte{[]byte{0x65, 0x88, 0x80, 0x40}})
	if !(h264Detector{}).IsKeyframe(p) {
		t.Error("AVCC IDR packet should be keyframe")
	}
}

func TestH264NonKeyframeAVCC(t *testing.T) {
	// Non-IDR slice (0x41) -> not a keyframe.
	p := h264AVCCPayload([][]byte{[]byte{0x41, 0x9A, 0x10, 0x20}})
	if (h264Detector{}).IsKeyframe(p) {
		t.Error("AVCC non-IDR packet should not be keyframe")
	}
}

func TestH264KeyframeAmongMultipleNALs(t *testing.T) {
	// SPS(0x67) + PPS(0x68) + IDR(0x65): packet is a keyframe because IDR present.
	p := h264AVCCPayload([][]byte{
		{0x67, 0x42, 0x00, 0x1E}, // SPS
		{0x68, 0xCE, 0x38, 0x80}, // PPS
		{0x65, 0x88, 0x80, 0x40}, // IDR
	})
	if !(h264Detector{}).IsKeyframe(p) {
		t.Error("packet with IDR NAL should be keyframe")
	}
}

func TestH264KeyframeAnnexB(t *testing.T) {
	// Annex B framing with start code 00 00 00 01 + IDR(0x65).
	p := h264AnnexBPayload([][]byte{[]byte{0x65, 0x88, 0x80, 0x40}})
	if !(h264Detector{}).IsKeyframe(p) {
		t.Error("Annex B IDR packet should be keyframe")
	}
}

func TestH264NonKeyframeAnnexB(t *testing.T) {
	p := h264AnnexBPayload([][]byte{[]byte{0x41, 0x9A, 0x10, 0x20}})
	if (h264Detector{}).IsKeyframe(p) {
		t.Error("Annex B non-IDR packet should not be keyframe")
	}
}

func TestH264AnnexB3ByteStartCode(t *testing.T) {
	// 3-byte start code 00 00 01 + IDR.
	p := []byte{0x00, 0x00, 0x01, 0x65, 0x88, 0x80, 0x40}
	if !(h264Detector{}).IsKeyframe(p) {
		t.Error("Annex B 3-byte start code IDR should be keyframe")
	}
}

func TestH264EmptyAndTruncated(t *testing.T) {
	// Empty, nil, and truncated inputs MUST NOT panic and report non-keyframe.
	cases := [][]byte{nil, {}, {0x00}, {0x00, 0x00}, {0x00, 0x00, 0x01}}
	for _, p := range cases {
		if (h264Detector{}).IsKeyframe(p) {
			t.Errorf("truncated input %v reported as keyframe", p)
		}
	}
}

func TestH264ForbiddenBit(t *testing.T) {
	// forbidden_zero_bit set (0xE5) -> malformed, must not be a keyframe.
	p := h264AVCCPayload([][]byte{[]byte{0xE5, 0x88, 0x80, 0x40}})
	if (h264Detector{}).IsKeyframe(p) {
		t.Error("NAL with forbidden bit set should not be keyframe")
	}
}

func TestH264MalformedAVCCLength(t *testing.T) {
	// Length prefix larger than remaining data -> must not panic.
	p := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x65}
	if (h264Detector{}).IsKeyframe(p) {
		t.Error("malformed AVCC length should not report keyframe")
	}
}

// HEVC NAL header (ITU-T H.265 section 7.3.1):
//   byte0: forbidden_zero_bit(1) | nal_unit_type(6) | nuh_layer_id_high(1)
//   byte1: nuh_layer_id_low(5) | nuh_temporal_id_plus1(3)
//
// IDR_N_LP (nal_unit_type=20): byte0 = 0 010100 0 = 0x28, byte1 = 0x01
// CRA (nal_unit_type=21): byte0 = 0 010101 0 = 0x2A, byte1 = 0x01
// TRAIL_R (nal_unit_type=1): byte0 = 0 000001 0 = 0x02, byte1 = 0x01

func TestHEVCKeyframeAVCC(t *testing.T) {
	// IDR_N_LP (type 20): 0x28 0x01 + slice data.
	p := h264AVCCPayload([][]byte{[]byte{0x28, 0x01, 0xAA, 0xBB}})
	if !(hevcDetector{}).IsKeyframe(p) {
		t.Error("AVCC HEVC IDR packet should be keyframe")
	}
}

func TestHEVCCRAKeyframe(t *testing.T) {
	// CRA (type 21) is an IRAP sync frame.
	p := h264AVCCPayload([][]byte{[]byte{0x2A, 0x01, 0xAA, 0xBB}})
	if !(hevcDetector{}).IsKeyframe(p) {
		t.Error("AVCC HEVC CRA packet should be keyframe")
	}
}

func TestHEVCNonKeyframe(t *testing.T) {
	// TRAIL_R (type 1): not a sync frame.
	p := h264AVCCPayload([][]byte{[]byte{0x02, 0x01, 0xAA, 0xBB}})
	if (hevcDetector{}).IsKeyframe(p) {
		t.Error("HEVC TRAIL_R packet should not be keyframe")
	}
}

func TestHEVCKeyframeAnnexB(t *testing.T) {
	p := h264AnnexBPayload([][]byte{[]byte{0x28, 0x01, 0xAA, 0xBB}})
	if !(hevcDetector{}).IsKeyframe(p) {
		t.Error("Annex B HEVC IDR packet should be keyframe")
	}
}

func TestHEVCEmptyAndTruncated(t *testing.T) {
	cases := [][]byte{nil, {}, {0x28}, {0x00, 0x00, 0x01}}
	for _, p := range cases {
		if (hevcDetector{}).IsKeyframe(p) {
			t.Errorf("truncated HEVC input %v reported as keyframe", p)
		}
	}
}

func TestHEVCForbiddenBit(t *testing.T) {
	// forbidden_zero_bit set (0x80 | 0x28 = 0xA8) -> malformed.
	p := h264AVCCPayload([][]byte{[]byte{0xA8, 0x01, 0xAA, 0xBB}})
	if (hevcDetector{}).IsKeyframe(p) {
		t.Error("HEVC NAL with forbidden bit set should not be keyframe")
	}
}

// Verify the exact header byte values are spec-derived (not just "contains").
func TestNALHeaderBytesExact(t *testing.T) {
	// H.264 IDR: 0 11 00101 = 0x65
	if b := byte(0x65); b&0x1F != 5 || b&0x80 != 0 {
		t.Error("H.264 IDR byte 0x65 not spec-derived")
	}
	// HEVC IDR_N_LP type 20: byte0 = 0 010100 0 = 0x28, type = (0x28>>1)&0x3F = 20
	if typ := (byte(0x28) >> 1) & 0x3F; typ != 20 {
		t.Errorf("HEVC byte 0x28 type = %d want 20", typ)
	}
}
