package puremux

import (
	"bytes"
	"math/rand"
	"slices"
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

// bframe60fps returns `groups` mini-GOPs of a 60fps decode-order PTS pattern
// (ms) with reorder depth 2: for base b at 50ms spacing, b+34, b+50, b+17
// (two anchors, then the B-frame that presents between them).
func bframe60fps(startMs, groups int) []int {
	var pts []int
	for g := range groups {
		b := startMs + 50*g
		pts = append(pts, b+34, b+50, b+17)
	}
	return pts
}

func TestWriteVideoReorderedJitteryStartup(t *testing.T) {
	// The v0.0.8 regression: a session starts with a backfill burst delivered
	// out of order with a duplicated timestamp (the IDR first, so the Aligner
	// drops nothing), then settles into a clean B-frame GOP. The v0.0.7
	// one-shot startup probe measured the reorder depth from the scrambled
	// burst, so whether the stream lead-shifted depended on the burst's
	// arrival order: most orderings produced DTS==PTS passthrough with
	// duplicate/non-monotonic DTS for the whole stream. Every scramble must
	// now produce a stream whose EVERY frame has strictly increasing DTS (at
	// 90 kHz granularity) and DTS <= PTS, with decode order and PTS deltas
	// preserved exactly.
	base := []int{13867, 13884, 13900, 13917, 13934, 13950, 13967, 13984}
	for seed := range int64(100) {
		rng := rand.New(rand.NewSource(seed))
		burst := slices.Clone(base)
		burst[rng.Intn(len(burst))] = burst[rng.Intn(len(burst))] // duplicate one
		rng.Shuffle(len(burst), func(i, j int) { burst[i], burst[j] = burst[j], burst[i] })
		if slices.IsSorted(burst) {
			// A shuffle that lands monotonic is indistinguishable from a true
			// reorder-free start; only actual scrambles are asserted here.
			continue
		}
		ptsMs := append(slices.Clone(burst), bframe60fps(14000, 80)...)

		var buf bytes.Buffer
		s, err := NewSession(&buf, reorderCfg())
		if err != nil {
			t.Fatal(err)
		}
		vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
		if err != nil {
			t.Fatal(err)
		}
		for i, ms := range ptsMs {
			hdr := byte(0x41)
			if i == 0 {
				hdr = 0x65 // IDR leads the backfill burst
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
			t.Fatalf("seed %d: video PES count = %d, want %d", seed, len(video), len(ptsMs))
		}
		for i, pes := range video {
			if pes.es[5] != byte(i) {
				t.Fatalf("seed %d: decode order broken at %d: payload tag %#x", seed, i, pes.es[5])
			}
			if i > 0 && pes.dts <= video[i-1].dts {
				t.Fatalf("seed %d: DTS not strictly increasing at %d: %d <= %d (burst %v)",
					seed, i, pes.dts, video[i-1].dts, burst)
			}
			if pes.dts > pes.pts {
				t.Fatalf("seed %d: DTS > PTS at %d: %d > %d (burst %v)", seed, i, pes.dts, pes.pts, burst)
			}
			wantDelta := uint64(ptsMs[i]-ptsMs[0]) * 90
			if got := pes.pts - video[0].pts; got != wantDelta {
				t.Fatalf("seed %d: PTS delta altered at %d: got %d ticks, want %d", seed, i, got, wantDelta)
			}
		}
	}
}

func TestWriteVideoReorderedDuplicateBurstThenBFrames(t *testing.T) {
	// The production jittery-startup shape: a backfill burst all sharing one
	// timestamp (no ordering evidence), then the clean B-frame pattern. The
	// probe must hold past the duplicate burst until ordering evidence
	// arrives instead of locking DTS==PTS passthrough for the whole stream.
	ptsMs := append([]int{13900, 13900, 13900, 13900, 13900, 13900, 13900, 13900},
		bframe60fps(14000, 80)...)
	var buf bytes.Buffer
	s, _ := NewSession(&buf, reorderCfg())
	vid, _ := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	for i, ms := range ptsMs {
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
	if len(video) != len(ptsMs) {
		t.Fatalf("video PES count = %d, want %d", len(video), len(ptsMs))
	}
	for i, pes := range video {
		if pes.es[5] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if i > 0 && pes.dts <= video[i-1].dts {
			t.Fatalf("DTS not strictly increasing at %d: %d <= %d", i, pes.dts, video[i-1].dts)
		}
		if pes.dts > pes.pts {
			t.Fatalf("DTS > PTS at %d: %d > %d", i, pes.dts, pes.pts)
		}
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
