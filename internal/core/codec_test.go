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
	if !d.IsKeyframe([]byte{0x00, 0x00, 0x00}) {
		t.Error("0x00 should be VP8 keyframe")
	}
	if d.IsKeyframe([]byte{0x01, 0x00, 0x00}) {
		t.Error("0x01 should be VP8 interframe")
	}
	if d.IsKeyframe(nil) {
		t.Error("nil should not be keyframe")
	}
}

func TestVP9Detector(t *testing.T) {
	d := vp9Detector{}
	// frame_marker 0b10 => low two bits == 0x02. Profile 0,
	// show_existing_frame=0, frame_type=0 (KEY) at bit 5 => byte 0x02.
	if !d.IsKeyframe([]byte{0x02}) {
		t.Error("0x02 should be VP9 keyframe")
	}
	// frame_type=1 (INTER) at bit 5 => byte 0x22.
	if d.IsKeyframe([]byte{0x22}) {
		t.Error("0x22 should be VP9 interframe")
	}
	if d.IsKeyframe([]byte{0x00}) {
		t.Error("invalid frame_marker should not be keyframe")
	}
	if d.IsKeyframe(nil) {
		t.Error("nil should not be keyframe")
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
	if !r.Detector(CodecVP9).IsKeyframe([]byte{0x02}) {
		t.Error("VP9 detector not wired in registry")
	}
	r.Register(CodecVP9, nil)
	if r.Detector(CodecVP9).IsKeyframe([]byte{0x02}) {
		t.Error("cleared VP9 detector should fall back to noop")
	}
}
