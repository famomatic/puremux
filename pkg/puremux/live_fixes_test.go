package puremux

import (
	"bytes"
	"io"
	"testing"
)

// TestMPEGTSDefaultConfigMonotonicDTS pins the fix for the DefaultConfig()
// footgun: MinMonotonicStep defaults to 0 (→ a 1 ns nudge), which rounds to 0
// ticks at the 90 kHz TS clock, so a duplicate-stamped burst produced identical
// (non-monotonic) PES DTS. NewSession now clamps the step to >= 1 tick for TS.
func TestMPEGTSDefaultConfigMonotonicDTS(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig() // MinMonotonicStep left 0 on purpose
	cfg.OutputContainer = ContainerMPEGTS
	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true, Width: 320, Height: 180})
	if err != nil {
		t.Fatal(err)
	}
	// A backfill burst: 6 AUs all stamped t=0. First is an IDR keyframe so the
	// Aligner keeps the rest.
	aus := [][]byte{
		annexBAU(0x65, 0x10), annexBAU(0x41, 0x11), annexBAU(0x41, 0x12),
		annexBAU(0x41, 0x13), annexBAU(0x41, 0x14), annexBAU(0x41, 0x15),
	}
	for _, au := range aus {
		if err := s.WriteVideo(vid, au, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	pes := demuxTS(t, buf.Bytes(), 0x100)
	if len(pes) < 2 {
		t.Fatalf("want >=2 PES, got %d", len(pes))
	}
	for i := 1; i < len(pes); i++ {
		if pes[i].dts <= pes[i-1].dts {
			t.Fatalf("PES DTS not strictly increasing at %d: %d <= %d (DefaultConfig step not clamped to a TS tick)", i, pes[i].dts, pes[i-1].dts)
		}
	}
}

// TestWriteADTSAfterCloseGuarded pins the closed-session guard on WriteADTS
// (previously missing — it acquired a pooled packet, then WritePacket returned
// ErrClosedPipe without releasing it, leaking one packet per call).
func TestWriteADTSAfterCloseGuarded(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMPEGTS
	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := s.AddTrack(Track{Codec: CodecAAC, Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteADTS(aud, adts48k([]byte{0x01, 0x02, 0x03}), 0); err != io.ErrClosedPipe {
		t.Fatalf("WriteADTS after Close = %v, want io.ErrClosedPipe", err)
	}
}
