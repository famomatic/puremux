package puremux

import (
	"bytes"
	"testing"
	"time"
)

// reorderCfg is the downstream live-caller configuration (MPEG-TS output,
// 1ms monotonic step surviving 90 kHz quantization).
func reorderCfg() Config {
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMPEGTS
	cfg.Preprocessor.MinMonotonicStep = uint64(time.Millisecond)
	return cfg
}

// ipbbDecodeOrder builds a decode-order PTS sequence (in ms) of `groups`
// IPBB mini-GOPs at a 10ms frame interval starting at base: each group is
// P(+30) followed by the two B-frames it references, presentation base <
// B1 < B2 < P.
func ipbbDecodeOrder(base, groups int) []int {
	pts := []int{base}
	for range groups {
		p := base + 30
		pts = append(pts, p, base+10, base+20)
		base = p
	}
	return pts
}

func TestWriteVideoReorderedBFrames(t *testing.T) {
	// The real-capture shape: H.264 with B-frames fed per-AU in decode order
	// with presentation timestamps only (first IDR at 10s like a live-edge
	// join). The output decode timeline must be strictly monotonic with
	// DTS <= PTS and untouched PTS, in decode order.
	ptsMs := ipbbDecodeOrder(10_000, 10)

	var buf bytes.Buffer
	s, err := NewSession(&buf, reorderCfg())
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true, Width: 2560, Height: 1440})
	if err != nil {
		t.Fatal(err)
	}
	for i, ms := range ptsMs {
		hdr := byte(0x41)
		if i == 0 {
			hdr = 0x65 // IDR
		}
		if err := s.WriteVideoReordered(vid, annexBAU(hdr, byte(i)), time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	video := demuxTS(t, buf.Bytes(), 0x100)
	if len(video) != len(ptsMs) {
		t.Fatalf("video PES count = %d, want %d", len(video), len(ptsMs))
	}
	sawSplitPTSDTS := false
	for i, pes := range video {
		// Payload (decode) order preserved exactly.
		if pes.es[5] != byte(i) {
			t.Fatalf("decode order broken at %d: payload tag %#x", i, pes.es[5])
		}
		// DTS strictly monotonic at 90 kHz granularity; DTS never after PTS.
		if i > 0 && video[i].dts <= video[i-1].dts {
			t.Fatalf("DTS not strictly increasing at %d: %d <= %d", i, video[i].dts, video[i-1].dts)
		}
		if pes.dts > pes.pts {
			t.Fatalf("DTS > PTS at %d: %d > %d", i, pes.dts, pes.pts)
		}
		if pes.hasDTS {
			sawSplitPTSDTS = true
		}
		// PTS preserved: the rebased PES PTS must keep the input deltas.
		wantDelta := uint64(ptsMs[i]-ptsMs[0]) * 90
		if got := pes.pts - video[0].pts; got != wantDelta {
			t.Fatalf("PTS delta altered at %d: got %d ticks, want %d", i, got, wantDelta)
		}
	}
	if !sawSplitPTSDTS {
		t.Fatal("no PES carried a distinct PTS/DTS pair; DTS synthesis inactive?")
	}
}

func TestWriteVideoReorderedMonotonicByteIdentical(t *testing.T) {
	// The unconditional-use guarantee: a stream with no reordering must come
	// out byte-identical to the plain WriteVideo path.
	writeAll := func(w func(int, []byte, time.Duration) error, vid int) {
		t.Helper()
		for i := range 12 {
			hdr := byte(0x41)
			if i == 0 {
				hdr = 0x65
			}
			if err := w(vid, annexBAU(hdr, byte(i)), time.Duration(100+20*i)*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	var plain, reordered bytes.Buffer
	s1, _ := NewSession(&plain, reorderCfg())
	vid1, _ := s1.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	writeAll(s1.WriteVideo, vid1)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewSession(&reordered, reorderCfg())
	vid2, _ := s2.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	writeAll(s2.WriteVideoReordered, vid2)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Bytes(), reordered.Bytes()) {
		t.Fatal("monotonic input through WriteVideoReordered is not byte-identical to WriteVideo")
	}
}

func TestWriteVideoReorderedDuplicateBurstByteIdentical(t *testing.T) {
	// The SOOP startup pathology (keyframe + followers sharing one backfill
	// timestamp) must keep receiving the Enforcer's duplicate repair —
	// byte-identical to the WriteVideo path, PTS nudges included.
	writeAll := func(w func(int, []byte, time.Duration) error, vid int) {
		t.Helper()
		t0 := 500 * time.Millisecond
		if err := w(vid, annexBAU(0x65, 0x00), t0); err != nil {
			t.Fatal(err)
		}
		for i := byte(1); i < 5; i++ {
			if err := w(vid, annexBAU(0x41, i), t0); err != nil {
				t.Fatal(err)
			}
		}
		for i := 1; i <= 3; i++ {
			if err := w(vid, annexBAU(0x41, 0xF0+byte(i)), t0+time.Duration(i)*20*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	var plain, reordered bytes.Buffer
	s1, _ := NewSession(&plain, reorderCfg())
	vid1, _ := s1.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	writeAll(s1.WriteVideo, vid1)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewSession(&reordered, reorderCfg())
	vid2, _ := s2.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	writeAll(s2.WriteVideoReordered, vid2)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Bytes(), reordered.Bytes()) {
		t.Fatal("duplicate-burst input through WriteVideoReordered is not byte-identical to WriteVideo")
	}
}

func TestWriteVideoReorderedCloseDrainsProbe(t *testing.T) {
	// A stream shorter than the startup probe: every frame is still held when
	// Close runs, which must drain them (header included) rather than lose them.
	var buf bytes.Buffer
	s, _ := NewSession(&buf, reorderCfg())
	vid, _ := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	// Decode order I, P, B with presentation I < B < P.
	for i, ms := range []int{100, 130, 110} {
		hdr := byte(0x41)
		if i == 0 {
			hdr = 0x65
		}
		if err := s.WriteVideoReordered(vid, annexBAU(hdr, byte(i)), time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	video := demuxTS(t, buf.Bytes(), 0x100)
	if len(video) != 3 {
		t.Fatalf("video PES count = %d, want 3 (probe tail lost on Close?)", len(video))
	}
	for i, pes := range video {
		if pes.es[5] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if i > 0 && pes.dts <= video[i-1].dts {
			t.Fatalf("DTS not strictly increasing at %d", i)
		}
		if pes.dts > pes.pts {
			t.Fatalf("DTS > PTS at %d", i)
		}
	}
}

func TestWriteVideoReorderedWithAudio(t *testing.T) {
	// Audio written while the video probe still holds frames must survive via
	// the Aligner pending queue and come out after the video sync point.
	ptsMs := ipbbDecodeOrder(1_000, 6)
	var buf bytes.Buffer
	s, _ := NewSession(&buf, reorderCfg())
	vid, _ := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	aud, _ := s.AddTrack(Track{Codec: CodecAAC, Channels: 2, SampleRate: 48000})
	for i, ms := range ptsMs {
		hdr := byte(0x41)
		if i == 0 {
			hdr = 0x65
		}
		if err := s.WriteVideoReordered(vid, annexBAU(hdr, byte(i)), time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteADTS(aud, adts48k([]byte{byte(i), 0x01}), time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	video := demuxTS(t, buf.Bytes(), 0x100)
	if len(video) != len(ptsMs) {
		t.Fatalf("video PES count = %d, want %d", len(video), len(ptsMs))
	}
	audio := demuxTS(t, buf.Bytes(), 0x101)
	if len(audio) == 0 {
		t.Fatal("all audio lost while the video probe was holding frames")
	}
}
