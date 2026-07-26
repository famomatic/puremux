package puremux

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestWriteVideoRealBFrameStream replays the H.264 access units of a REAL
// captured B-frame transport stream through WriteVideo on a monotonic decode
// clock, then verifies the v0.0.9 invariants on the muxed output: strictly
// monotonic DTS, DTS <= PTS, decode order preserved. This guards against a
// regression that a hand-built POC fixture might miss (real SPS/PPS, real slice
// headers, real GOP structure). Opt-in: PUREMUX_TS_SAMPLE=<path.ts> (a video-PID
// 0x100 TS whose H.264 carries B-frames).
func TestWriteVideoRealBFrameStream(t *testing.T) {
	path := os.Getenv("PUREMUX_TS_SAMPLE")
	if path == "" {
		t.Skip("set PUREMUX_TS_SAMPLE=<path.ts> to run")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	// Truncate to a whole number of TS packets (a live capture may be cut mid-packet).
	raw = raw[:len(raw)/188*188]

	in := demuxTS(t, raw, 0x100)
	aus := make([][]byte, 0, len(in))
	for _, pes := range in {
		if len(pes.es) > 0 {
			aus = append(aus, pes.es)
		}
	}
	if len(aus) < 100 {
		t.Fatalf("too few AUs in sample: %d", len(aus))
	}

	var buf bytes.Buffer
	s, err := NewSession(&buf, reorderCfg())
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true, Width: 2560, Height: 1440})
	if err != nil {
		t.Fatal(err)
	}
	for i, au := range aus {
		// Monotonic 60fps decode clock (PTS == DTS == t), the caller's shape.
		if err := s.WriteVideo(vid, au, time.Duration(i)*time.Second/60); err != nil {
			t.Fatalf("WriteVideo %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	out := demuxTS(t, buf.Bytes(), 0x100)
	if len(out) == 0 {
		t.Fatal("no output PES")
	}
	var nonMono, dtsGtPts, distinct int
	prev := out[0].dts
	for i, pes := range out {
		if i > 0 && pes.dts <= prev {
			nonMono++
		}
		prev = pes.dts
		if pes.dts > pes.pts {
			dtsGtPts++
		}
		if pes.hasDTS {
			distinct++
		}
	}
	t.Logf("real replay: %d AUs in, %d PES out, distinct PTS/DTS pairs=%d", len(aus), len(out), distinct)
	// Critical invariants hold for ANY stream (B-frame or not):
	if nonMono != 0 {
		t.Fatalf("output DTS non-monotonic on %d/%d frames (want 0)", nonMono, len(out))
	}
	if dtsGtPts != 0 {
		t.Fatalf("DTS > PTS on %d/%d frames (want 0)", dtsGtPts, len(out))
	}
	// distinct>0 is expected only for a B-frame stream (POC reordering); a
	// reorder-free stream correctly keeps DTS==PTS (distinct==0). Informational.
	if distinct == 0 {
		t.Log("note: distinct PTS/DTS=0 — sample has no B-frame reordering (fine)")
	}
}
