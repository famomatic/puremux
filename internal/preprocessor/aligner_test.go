package preprocessor

import (
	"testing"
	"time"

	"famomatic/puremux/internal/core"
)

// fakeDetector is a test-only CodecKeyframeDetector returning a fixed result.
type fakeDetector struct{ key bool }

func (f fakeDetector) IsKeyframe([]byte) bool { return f.key }

func TestAlignerVideoDropsUntilKeyframe(t *testing.T) {
	// Detector reports non-keyframe; aligner must drop until IsKeyframe true.
	a := NewAligner(fakeDetector{key: false}, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// Two non-keyframe packets before the keyframe.
	for i := 0; i < 2; i++ {
		p := core.AcquirePacket()
		p.DTS = time.Duration(i) * time.Millisecond
		p.Codec = core.CodecVP9
		a.Process(p, emit)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 emitted before keyframe, got %d", len(got))
	}
	if a.Metrics().DroppedOutOfOrder != 2 {
		t.Errorf("expected 2 drops, got %d", a.Metrics().DroppedOutOfOrder)
	}

	// Now a packet flagged as keyframe (IsKeyframe field overrides detector).
	kf := core.AcquirePacket()
	kf.DTS = 2 * time.Millisecond
	kf.IsKeyframe = true
	kf.Codec = core.CodecVP9
	a.Process(kf, emit)
	if len(got) != 1 {
		t.Errorf("expected keyframe emitted, got %d", len(got))
	}

	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerVideoPassesAfterKeyframe(t *testing.T) {
	a := NewAligner(fakeDetector{key: true}, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// First packet is keyframe (detector says true).
	p1 := core.AcquirePacket()
	p1.DTS = 0
	a.Process(p1, emit)

	// Subsequent non-keyframe packets pass through.
	p2 := core.AcquirePacket()
	p2.DTS = 10 * time.Millisecond
	a.Process(p2, emit)

	if len(got) != 2 {
		t.Errorf("expected 2 emitted, got %d", len(got))
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerAudioPacketGranularDrop(t *testing.T) {
	// Audio aligner: once sync start is set, drop whole packets before it.
	a := NewAligner(fakeDetector{}, false)
	a.SetVideoSyncStart(50 * time.Millisecond)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// Packet before sync start -> dropped (whole packet, no trim).
	early := core.AcquirePacket()
	early.DTS = 20 * time.Millisecond
	early.Data = []byte{0xAA, 0xBB, 0xCC}
	a.Process(early, emit)
	if a.Metrics().AudioPacketsDropped != 1 {
		t.Errorf("expected 1 audio drop, got %d", a.Metrics().AudioPacketsDropped)
	}
	if len(got) != 0 {
		t.Errorf("early audio should be dropped, got %d", len(got))
	}

	// Packet at sync start -> emitted unchanged (no trimming).
	onTime := core.AcquirePacket()
	onTime.DTS = 50 * time.Millisecond
	onTime.Data = []byte{0xDD, 0xEE}
	a.Process(onTime, emit)
	if len(got) != 1 {
		t.Fatalf("expected 1 emitted, got %d", len(got))
	}
	// Verify the packet was NOT trimmed: data intact.
	if string(got[0].Data) != "\xDD\xEE" {
		t.Error("audio packet data was altered (trim forbidden by §5.B)")
	}

	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerAudioOnlyNoSyncStart(t *testing.T) {
	// Audio-only stream: no video sync start set, all packets pass through.
	a := NewAligner(fakeDetector{}, false)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	for i := 0; i < 3; i++ {
		p := core.AcquirePacket()
		p.DTS = time.Duration(i) * time.Millisecond
		a.Process(p, emit)
	}
	if len(got) != 3 {
		t.Errorf("audio-only should pass all, got %d", len(got))
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerReset(t *testing.T) {
	a := NewAligner(fakeDetector{key: true}, true)
	emit := func(p *core.Packet) { core.ReleasePacket(p) }
	p := core.AcquirePacket()
	p.DTS = 0
	a.Process(p, emit)
	a.Reset()
	if a.Metrics().DroppedOutOfOrder != 0 {
		t.Error("Reset should clear metrics")
	}
}