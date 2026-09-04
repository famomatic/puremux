package core

import "testing"

func TestCodecTypeString(t *testing.T) {
	cases := []struct {
		c    CodecType
		want string
		vid  bool
	}{
		{CodecVP8, "vp8", true},
		{CodecVP9, "vp9", true},
		{CodecAV1, "av1", true},
		{CodecOpus, "opus", false},
		{CodecUnknown, "unknown", false},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("%d String = %q want %q", tc.c, got, tc.want)
		}
		if got := tc.c.IsVideo(); got != tc.vid {
			t.Errorf("%d IsVideo = %v want %v", tc.c, got, tc.vid)
		}
	}
}

func TestPacketReset(t *testing.T) {
	p := &Packet{
		Data:       []byte{1, 2, 3},
		PTS:        42,
		DTS:        41,
		IsKeyframe: true,
		Codec:      CodecVP9,
		TrackID:    7,
	}
	p.Reset()
	if p.PTS != 0 || p.DTS != 0 || p.IsKeyframe || p.Codec != CodecUnknown || p.TrackID != 0 {
		t.Fatalf("Reset left state: %+v", p)
	}
	if cap(p.Data) == 0 {
		t.Fatal("Reset should retain Data backing capacity")
	}
}

func TestVP8Detector(t *testing.T) {
	d := vp8Detector{}
	// RFC 6386 9.1: bit0 of frame tag = frame_type, 0 = keyframe.
	key := []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x10, 0x00, 0x10, 0x00}
	if !d.IsKeyframe(key) {
		t.Error("spec-shaped VP8 key frame was not detected")
	}
	if d.IsKeyframe([]byte{0x01, 0x00, 0x00}) {
		t.Error("0x01 should be VP8 interframe")
	}
	if d.IsKeyframe(nil) {
		t.Error("nil should not be keyframe")
	}
	if d.IsKeyframe([]byte{0x00}) || d.IsKeyframe([]byte{0x00, 0x00, 0x00, 0, 0, 0}) {
		t.Error("truncated or bad-sync VP8 data must not be a keyframe")
	}
}

func TestVP9Detector(t *testing.T) {
	d := vp9Detector{}
	// VP9 header is MSB-first. Profile 0 keyframe byte:
	//   bits 10 00 0 0 xx = frame_marker(10) profLow(0) profHigh(0)
	//   show_existing_frame(0) frame_type(0=KEY) => 0b10000000 = 0x80.
	if !d.IsKeyframe([]byte{0x80}) {
		t.Error("0x80 should be VP9 keyframe")
	}
	// frame_type=1 (INTER): bit2 set => 0b10000100 = 0x84.
	if d.IsKeyframe([]byte{0x84}) {
		t.Error("0x84 should be VP9 interframe")
	}
	if d.IsKeyframe([]byte{0x00}) {
		t.Error("invalid frame_marker should not be keyframe")
	}
	if d.IsKeyframe(nil) {
		t.Error("nil should not be keyframe")
	}
}

// TestVP9DetectorProfile3 verifies the detector handles profile 3, which
// inserts an extra reserved_zero bit between profile_high_bit and
// show_existing_frame, shifting frame_type by one bit position.
func TestVP9DetectorProfile3(t *testing.T) {
	d := vp9Detector{}
	// profile 3: profile_low=1, profile_high=1. MSB-first bit layout:
	//   [7:6]=10(marker) [5]=1(profLow) [4]=1(profHigh) [3]=0(reserved)
	//   [2]=0(show_existing_frame) [1]=0(frame_type=KEY) => 0b10110000 = 0xB0.
	if !d.IsKeyframe([]byte{0xB0}) {
		t.Error("profile 3 KEY_FRAME (0xB0) should be keyframe")
	}
	// profile 3 INTER: frame_type=1 at bit1 => 0b10110010 = 0xB2.
	if d.IsKeyframe([]byte{0xB2}) {
		t.Error("profile 3 INTER (0xB2) should not be keyframe")
	}
	// profile 3 show_existing_frame=1 at bit2 => 0b10110100 = 0xB4.
	// show_existing_frame references a prior frame, not a sync frame.
	if d.IsKeyframe([]byte{0xB4}) {
		t.Error("profile 3 show_existing_frame (0xB4) should not be keyframe")
	}
	if d.IsKeyframe([]byte{0xB8}) {
		t.Error("profile 3 reserved_zero=1 must be rejected")
	}
}

// AV1 OBU header byte builder (MSB-first, has_size=1, no extension).
func obuHeader(obuType int) byte {
	return byte((0 << 7) | (obuType << 3) | (0 << 2) | (1 << 1) | (0 << 0))
}

// AV1 frame_header payload byte for a given frame_type, MSB-first:
// bit7=show_existing_frame(0), bits[6:5]=frame_type.
func av1FramePayload(frameType int) byte {
	return byte((0 << 7) | (frameType << 5))
}

func TestAV1DetectorKeyframe(t *testing.T) {
	d := av1Detector{}
	// frame_type=0 (KEY_FRAME).
	pkt := []byte{obuHeader(obuFrameHeader), 0x01, av1FramePayload(0)}
	if !d.IsKeyframe(pkt) {
		t.Error("AV1 KEY_FRAME (frame_type=0) not detected as keyframe")
	}
}

func TestAV1DetectorInterframe(t *testing.T) {
	d := av1Detector{}
	// frame_type=1 (INTER): bits[6:5]=01 => 0b001_00000 = 0x20.
	pkt := []byte{obuHeader(obuFrameHeader), 0x01, av1FramePayload(1)}
	if d.IsKeyframe(pkt) {
		t.Error("AV1 INTER (frame_type=1) must not be keyframe")
	}
}

func TestAV1DetectorIntraOnly(t *testing.T) {
	d := av1Detector{}
	// frame_type=2 (INTRA_ONLY): bits[6:5]=10 => 0b010_00000 = 0x40.
	pkt := []byte{obuHeader(obuFrameHeader), 0x01, av1FramePayload(2)}
	if d.IsKeyframe(pkt) {
		t.Error("AV1 INTRA_ONLY (frame_type=2) must not be keyframe")
	}
}

func TestAV1DetectorShowExistingFrame(t *testing.T) {
	d := av1Detector{}
	// show_existing_frame=1: bit7=1 => 0b100_00000 = 0x80.
	pkt := []byte{obuHeader(obuFrameHeader), 0x01, 0x80}
	if d.IsKeyframe(pkt) {
		t.Error("AV1 show_existing_frame must not be reported as keyframe")
	}
}

func TestAV1DetectorSequenceHeaderThenFrame(t *testing.T) {
	d := av1Detector{}
	// Realistic shape: SEQUENCE_HEADER OBU followed by a KEY frame OBU.
	pkt := []byte{
		obuHeader(obuSequenceHeader), 0x01, 0x00,
		obuHeader(obuFrameHeader), 0x01, av1FramePayload(0),
	}
	if !d.IsKeyframe(pkt) {
		t.Error("AV1 seq-header + keyframe not detected")
	}
}

func TestAV1DetectorSequenceHeaderThenInter(t *testing.T) {
	d := av1Detector{}
	// Sequence header followed by INTER must NOT be a keyframe.
	pkt := []byte{
		obuHeader(obuSequenceHeader), 0x01, 0x00,
		obuHeader(obuFrameHeader), 0x01, av1FramePayload(1),
	}
	if d.IsKeyframe(pkt) {
		t.Error("AV1 seq-header + inter must not be keyframe")
	}
}

func TestAV1DetectorSequenceHeaderOnly(t *testing.T) {
	d := av1Detector{}
	pkt := []byte{obuHeader(obuSequenceHeader), 0x01, 0x00}
	if d.IsKeyframe(pkt) {
		t.Error("AV1 packet without frame header should not report keyframe")
	}
}

func TestAV1DetectorReducedStillPictureHeader(t *testing.T) {
	d := av1Detector{}
	// Sequence header first byte: seq_profile=000, still_picture=0,
	// reduced_still_picture_header=1 (bit 3), MSB-first.
	pkt := []byte{
		obuHeader(obuSequenceHeader), 0x01, 0x08,
		obuHeader(obuFrameHeader), 0x01, 0x00,
	}
	if !d.IsKeyframe(pkt) {
		t.Fatal("reduced still picture is an implied key frame")
	}
}

func TestAV1DetectorRejectsReservedBits(t *testing.T) {
	d := av1Detector{}
	if d.IsKeyframe([]byte{obuHeader(obuFrameHeader) | 1, 1, 0}) {
		t.Fatal("OBU reserved bit accepted")
	}
	// Extension header low three bits are reserved_zero_3bits.
	if d.IsKeyframe([]byte{obuHeader(obuFrameHeader) | 0x04, 0x01, 1, 0}) {
		t.Fatal("OBU extension reserved bits accepted")
	}
}

func TestAV1DetectorTruncated(t *testing.T) {
	d := av1Detector{}
	cases := [][]byte{
		nil,
		{},
		{obuHeader(obuFrameHeader)},       // header but no size
		{obuHeader(obuFrameHeader), 0x01}, // size but no payload
		{0xFF},                            // forbidden bit set
	}
	for i, pkt := range cases {
		if d.IsKeyframe(pkt) {
			t.Errorf("case %d: truncated/invalid packet must not be keyframe", i)
		}
	}
}

func TestAV1DetectorMultipleOBUsFirstNonFrame(t *testing.T) {
	d := av1Detector{}
	// Temporal delimiter (type 2) then metadata (type 5) then KEY frame.
	// The detector must skip non-frame OBUs and find the frame header.
	pkt := []byte{
		obuHeader(obuTemporalDelimiter), 0x00, // empty payload
		obuHeader(obuMetadata), 0x01, 0x00,
		obuHeader(obuFrameHeader), 0x01, av1FramePayload(0),
	}
	if !d.IsKeyframe(pkt) {
		t.Error("AV1 must skip leading non-frame OBUs to find keyframe")
	}
}

func TestDetectorRegistry(t *testing.T) {
	r := NewDetectorRegistry()
	if r.Detector(CodecOpus).IsKeyframe([]byte{0xFF}) {
		t.Error("Opus noop detector must never report keyframe")
	}
	if r.Detector(CodecUnknown).IsKeyframe([]byte{0xFF}) {
		t.Error("Unknown codec falls back to noop, not keyframe")
	}
	if !r.Detector(CodecVP9).IsKeyframe([]byte{0x80}) {
		t.Error("VP9 detector not wired in registry")
	}
	r.Register(CodecVP9, nil)
	if r.Detector(CodecVP9).IsKeyframe([]byte{0x80}) {
		t.Error("cleared VP9 detector should fall back to noop")
	}
}
