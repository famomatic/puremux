package core

import "testing"

func TestAcquireReleaseRoundtrip(t *testing.T) {
	p := AcquirePacket()
	if p == nil {
		t.Fatal("AcquirePacket returned nil")
	}
	if p.PTS != 0 || p.Codec != CodecUnknown || p.IsKeyframe {
		t.Fatalf("acquired packet not reset: %+v", p)
	}
	p.PTS = 99
	p.Codec = CodecVP9
	ReleasePacket(p)

	q := AcquirePacket()
	if q.PTS != 0 || q.Codec != CodecUnknown {
		t.Fatalf("re-acquired packet not reset: %+v", q)
	}
	ReleasePacket(q)
}

func TestReleaseNilSafe(t *testing.T) {
	// Should not panic.
	ReleasePacket(nil)
}

func TestTrackClassification(t *testing.T) {
	v := Track{Kind: TrackVideo, Codec: CodecVP9}
	a := Track{Kind: TrackAudio, Codec: CodecOpus}
	if !v.IsVideo() || v.IsAudio() {
		t.Error("video track misclassified")
	}
	if !a.IsAudio() || a.IsVideo() {
		t.Error("audio track misclassified")
	}
}