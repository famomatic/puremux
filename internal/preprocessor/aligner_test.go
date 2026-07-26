package preprocessor

import (
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
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

// TestAlignerAudioHoldsUntilVideoSync verifies that an audio aligner built
// with expectsVideoSync=true holds packets until SetVideoSyncStart arrives,
// then drops those before the sync point and emits those at/after it.
func TestAlignerAudioHoldsUntilVideoSync(t *testing.T) {
	a := NewAlignerForSession(fakeDetector{}, false, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// Audio before the video sync start: must be held, not emitted yet.
	early := core.AcquirePacket()
	early.DTS = 10 * time.Millisecond
	early.Data = []byte{0x01}
	a.Process(early, emit)
	if len(got) != 0 {
		t.Fatalf("audio before sync should be held, got %d emitted", len(got))
	}

	// Lock the video sync start at 50ms.
	a.SetVideoSyncStart(50 * time.Millisecond)

	// Audio at/after sync: emitted.
	onTime := core.AcquirePacket()
	onTime.DTS = 50 * time.Millisecond
	onTime.Data = []byte{0x02}
	a.Process(onTime, emit)

	// The held early packet (10ms < 50ms) must have been dropped, and the
	// on-time packet emitted: exactly one packet in got.
	if len(got) != 1 {
		t.Fatalf("after sync, want 1 emitted (on-time), got %d", len(got))
	}
	if string(got[0].Data) != "\x02" {
		t.Error("emitted packet was not the on-time packet")
	}
	if a.Metrics().AudioPacketsDropped != 1 {
		t.Errorf("want 1 dropped (the held early packet), got %d", a.Metrics().AudioPacketsDropped)
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

// TestAlignerAudioOnlySessionFlushesPending verifies that an audio-only
// session (no video track) does not hold packets indefinitely: SetExpectsVideoSync
// releases the held packets once the session determines it has no video.
func TestAlignerAudioOnlySessionFlushesPending(t *testing.T) {
	a := NewAlignerForSession(fakeDetector{}, false, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	p1 := core.AcquirePacket()
	p1.DTS = 10 * time.Millisecond
	a.Process(p1, emit)
	if len(got) != 0 {
		t.Fatal("held packet should not be emitted before decision")
	}

	// Session resolves to audio-only: flip the flag and flush.
	a.SetExpectsVideoSync(false, emit)
	if len(got) != 1 {
		t.Fatalf("audio-only should flush held packet, got %d", len(got))
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

// h264AU builds an Annex-B AU from raw NAL bodies (header byte included).
func h264AU(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, n...)
	}
	return out
}

var (
	nalSPS    = []byte{0x67, 0x42, 0x00, 0x1E, 0xEC, 0xA0, 0xA0, 0xFC}
	nalPPS    = []byte{0x68, 0xC8}
	nalIDR    = []byte{0x65, 0x88, 0x84, 0x08}
	nalNonIDR = []byte{0x41, 0xE2, 0x44}
)

func TestAlignerPreservesParameterSetsBeforeKeyframe(t *testing.T) {
	// A live join: standalone SPS/PPS AU arrives first, then pre-keyframe
	// P-frames (undecodable, dropped), then the IDR. The parameter sets must
	// come out ahead of the IDR instead of dying with the dropped frames —
	// without them the whole stream is undecodable when the IDR does not
	// repeat them in-band.
	det := core.NewDetectorRegistry().Detector(core.CodecH264)
	a := NewAligner(det, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	cfg := core.AcquirePacket()
	cfg.Data = append(cfg.Data[:0], h264AU(nalSPS, nalPPS)...)
	cfg.DTS = 0
	a.Process(cfg, emit)

	pre := core.AcquirePacket()
	pre.Data = append(pre.Data[:0], h264AU(nalNonIDR)...)
	pre.DTS = 10 * time.Millisecond
	a.Process(pre, emit)

	if len(got) != 0 {
		t.Fatalf("nothing may leak before the keyframe, got %d", len(got))
	}

	idr := core.AcquirePacket()
	idr.Data = append(idr.Data[:0], h264AU(nalIDR)...)
	idr.DTS = 20 * time.Millisecond
	a.Process(idr, emit)

	if len(got) != 2 {
		t.Fatalf("want cfg + IDR emitted, got %d packets", len(got))
	}
	if got[0] != cfg || got[1] != idr {
		t.Fatal("emission order wrong: parameter sets must precede the IDR")
	}
	if a.Metrics().DroppedOutOfOrder != 1 {
		t.Fatalf("want exactly the P-frame dropped, got %d", a.Metrics().DroppedOutOfOrder)
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerParameterSetQueueBounded(t *testing.T) {
	det := core.NewDetectorRegistry().Detector(core.CodecH264)
	a := NewAligner(det, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	for i := range maxCfgPending + 3 {
		cfg := core.AcquirePacket()
		cfg.Data = append(cfg.Data[:0], h264AU(nalSPS, nalPPS)...)
		cfg.DTS = time.Duration(i) * time.Millisecond
		a.Process(cfg, emit)
	}
	idr := core.AcquirePacket()
	idr.Data = append(idr.Data[:0], h264AU(nalIDR)...)
	a.Process(idr, emit)
	if len(got) != maxCfgPending+1 {
		t.Fatalf("want %d held cfg + IDR, got %d", maxCfgPending, len(got))
	}
	// The oldest overflowed; the newest configuration survived.
	if got[len(got)-2].DTS != time.Duration(maxCfgPending+2)*time.Millisecond {
		t.Fatal("bounded queue dropped the newest configuration instead of the oldest")
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestAlignerParameterSetsReleasedWithoutKeyframe(t *testing.T) {
	// Stream ends with configuration held but no keyframe ever: Flush must
	// not emit orphaned parameter sets (there is no stream to configure).
	det := core.NewDetectorRegistry().Detector(core.CodecH264)
	a := NewAligner(det, true)
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	cfg := core.AcquirePacket()
	cfg.Data = append(cfg.Data[:0], h264AU(nalSPS, nalPPS)...)
	a.Process(cfg, emit)
	a.Flush(emit)
	if len(got) != 0 {
		t.Fatalf("orphaned parameter sets emitted: %d", len(got))
	}
}
