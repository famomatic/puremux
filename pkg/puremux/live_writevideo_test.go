package puremux

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// --- hand-assembled H.264 AUs with real POC fields ---------------------------
//
// The WriteVideo B-frame tests need access units whose slice headers carry a
// real picture order count (the v0.0.9 pipeline derives PES PTS from it).
// Fixtures follow ITU-T H.264 §7.3.2.1/2, §7.3.3; field derivations are in
// internal/core/poc_test.go, which pins the same bytes against the spec.

type tbitWriter struct {
	buf []byte
	n   uint
}

func (w *tbitWriter) u(v uint32, bits uint) {
	for i := bits; i > 0; i-- {
		if w.n%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>(i-1)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> (w.n % 8)
		}
		w.n++
	}
}

func (w *tbitWriter) ue(v uint32) {
	k := v + 1
	bits := uint(0)
	for t := k; t > 0; t >>= 1 {
		bits++
	}
	w.u(0, bits-1)
	w.u(k, bits)
}

// tSPS/tPPS: baseline SPS (poc type 0, 6-bit poc_lsb, 4-bit frame_num) + PPS.
var (
	tSPS = []byte{0x67, 0x42, 0x00, 0x1E, 0xEC, 0xA0, 0xA0, 0xFC}
	tPPS = []byte{0x68, 0xC8}
)

// h264SliceAU builds one Annex-B AU holding a single slice NAL with the given
// POC LSB, plus a trailing tag byte (slice "data") to track decode order.
func h264SliceAU(idr, ref bool, frameNum, pocLsb uint32, tag byte, withParams bool) []byte {
	w := &tbitWriter{}
	w.ue(0) // first_mb_in_slice
	if idr {
		w.ue(7) // slice_type I
	} else {
		w.ue(0) // slice_type P (POC is what matters, not the type)
	}
	w.ue(0) // pps_id
	w.u(frameNum, 4)
	if idr {
		w.ue(0) // idr_pic_id
	}
	w.u(pocLsb, 6)
	w.u(1, 1) // rbsp stop bit
	hdr := byte(0x01)
	if idr {
		hdr = 0x05
	}
	if ref {
		hdr |= 0x60
	}
	nal := append([]byte{hdr}, w.buf...)
	nal = append(nal, tag)
	var au []byte
	if withParams {
		au = append(au, 0x00, 0x00, 0x00, 0x01)
		au = append(au, tSPS...)
		au = append(au, 0x00, 0x00, 0x00, 0x01)
		au = append(au, tPPS...)
	}
	au = append(au, 0x00, 0x00, 0x00, 0x01)
	au = append(au, nal...)
	return au
}

// bframeGOPAUs builds decode-order AUs of the x264 IPBB shape: IDR(poc 0),
// then per group P(+6, ref), B(+2, ref), b(+4, non-ref). The AU's tag byte is
// its decode index. Returns the AUs plus each AU's display rank.
func bframeGOPAUs(groups int) (aus [][]byte, displayRank []int) {
	type pic struct {
		order int64
		idx   int
	}
	var pics []pic
	aus = append(aus, h264SliceAU(true, true, 0, 0, 0, true))
	pics = append(pics, pic{order: 0, idx: 0})
	base := int64(0)
	fn := uint32(1)
	idx := 1
	for range groups {
		for _, d := range []struct {
			off int64
			ref bool
		}{{6, true}, {2, true}, {4, false}} {
			order := base + d.off
			aus = append(aus, h264SliceAU(false, d.ref, fn%16, uint32(order%64), byte(idx), false))
			pics = append(pics, pic{order: order, idx: idx})
			if d.ref {
				fn++
			}
			idx++
		}
		base += 6
	}
	byOrder := slices.Clone(pics)
	slices.SortFunc(byOrder, func(a, b pic) int {
		return int(a.order - b.order)
	})
	displayRank = make([]int, len(pics))
	for rank, p := range byOrder {
		displayRank[p.idx] = rank
	}
	return aus, displayRank
}

// muxWriteVideo drives the caller shape from the bug report: one WriteVideo
// per AU on a monotonic 60fps decode clock (PTS == DTS == t), TS output.
func muxWriteVideo(t *testing.T, aus [][]byte, clockMs []int) []pesInfo {
	t.Helper()
	var buf bytes.Buffer
	s, err := NewSession(&buf, reorderCfg())
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true, Width: 320, Height: 180})
	if err != nil {
		t.Fatal(err)
	}
	for i, au := range aus {
		if err := s.WriteVideo(vid, au, time.Duration(clockMs[i])*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return demuxTS(t, buf.Bytes(), 0x100)
}

func TestWriteVideoBFrameMonotonicDTS(t *testing.T) {
	// The v0.0.9 bug (B): H.264 with B-frames fed via WriteVideo on a
	// strictly monotonic decode clock produced a decode-order PES PTS with
	// no DTS — non-monotonic in display order, fatal for ffmpeg/mpv. The
	// output must now carry: strictly monotonic DTS == the input clock,
	// DTS <= PTS, decode order preserved, and a PES PTS that follows the
	// bitstream POC display order.
	aus, displayRank := bframeGOPAUs(20)
	clock := make([]int, len(aus))
	for i := range clock {
		clock[i] = i * 17 // 60fps-ish monotonic decode clock
	}
	video := muxWriteVideo(t, aus, clock)
	if len(video) != len(aus) {
		t.Fatalf("PES count = %d, want %d", len(video), len(aus))
	}
	sawSplit := false
	for i, pes := range video {
		if pes.es[len(pes.es)-1] != byte(i) {
			t.Fatalf("decode order broken at %d: tag %#x", i, pes.es[len(pes.es)-1])
		}
		if i > 0 && pes.dts <= video[i-1].dts {
			t.Fatalf("DTS not strictly increasing at %d: %d <= %d", i, pes.dts, video[i-1].dts)
		}
		if pes.dts > pes.pts {
			t.Fatalf("DTS > PTS at %d: %d > %d", i, pes.dts, pes.pts)
		}
		// DTS must be the input decode clock, rebased but delta-exact.
		wantDelta := uint64(clock[i]-clock[0]) * 90
		if got := pes.dts - video[0].dts; got != wantDelta {
			t.Fatalf("DTS delta altered at %d: got %d ticks, want %d", i, got, wantDelta)
		}
		if pes.hasDTS {
			sawSplit = true
		}
	}
	if !sawSplit {
		t.Fatal("no PES carried a distinct PTS/DTS pair; presentation synthesis inactive?")
	}
	// PES PTS must be strictly increasing in DISPLAY (POC) order, and — for
	// this depth-1 stream on a uniform clock — sit exactly at display slot
	// rank+1 of the decode clock (17ms steps), extrapolated tail included.
	byRank := make([]uint64, len(video))
	for i, pes := range video {
		byRank[displayRank[i]] = pes.pts
	}
	for k := 1; k < len(byRank); k++ {
		if byRank[k] <= byRank[k-1] {
			t.Fatalf("display-order PTS not strictly increasing at rank %d: %d <= %d",
				k, byRank[k], byRank[k-1])
		}
	}
	for k := range byRank {
		want := video[0].dts + uint64(17*(k+1))*90
		if byRank[k] != want {
			t.Fatalf("display slot %d PTS = %d ticks, want %d", k, byRank[k], want)
		}
	}
}

func TestWriteVideoBFrameJitteryStartupBurst(t *testing.T) {
	// The full production pathology: the first 8 AUs of the session arrive
	// with scrambled and duplicated decode-clock stamps (backfill burst),
	// then the clock is clean. For every seed, the muxed output must hold
	// the invariants for EVERY frame: strictly monotonic DTS at 90 kHz,
	// DTS <= PTS, no non-deterministic flakiness.
	aus, _ := bframeGOPAUs(20)
	for seed := range int64(60) {
		rng := rand.New(rand.NewSource(seed))
		clock := make([]int, len(aus))
		for i := range clock {
			clock[i] = i * 17
		}
		burst := clock[:8]
		burst[rng.Intn(len(burst))] = burst[rng.Intn(len(burst))] // duplicate one
		rng.Shuffle(len(burst), func(i, j int) { burst[i], burst[j] = burst[j], burst[i] })

		video := muxWriteVideo(t, aus, clock)
		if len(video) == 0 {
			t.Fatalf("seed %d: no video PES", seed)
		}
		// The Enforcer may reorder/drop within the scrambled burst (that is
		// its jitter-repair job); every frame that comes out must satisfy
		// the timeline invariants, and the steady tail must be complete.
		if len(video) < len(aus)-8 {
			t.Fatalf("seed %d: lost steady frames: %d of %d", seed, len(video), len(aus))
		}
		for i, pes := range video {
			if i > 0 && pes.dts <= video[i-1].dts {
				t.Fatalf("seed %d: DTS not strictly increasing at %d (burst %v)", seed, i, burst)
			}
			if pes.dts > pes.pts {
				t.Fatalf("seed %d: DTS > PTS at %d (burst %v)", seed, i, burst)
			}
		}
		// Steady tail: decode order intact (tags increasing).
		tail := video[len(video)-20:]
		for i := 1; i < len(tail); i++ {
			a := tail[i-1].es[len(tail[i-1].es)-1]
			b := tail[i].es[len(tail[i].es)-1]
			if b != a+1 {
				t.Fatalf("seed %d: steady decode order broken: tag %d after %d", seed, b, a)
			}
		}
	}
}

func TestWriteVideoNoBFramesStaysPTSOnly(t *testing.T) {
	// Reorder-free H.264 through WriteVideo must keep the old shape: every
	// PES PTS-only (PTS == DTS == t), zero drops, decode order intact.
	var aus [][]byte
	for i := range 24 {
		aus = append(aus, h264SliceAU(i == 0, true, uint32(i%16), uint32((2*i)%64), byte(i), i == 0))
	}
	clock := make([]int, len(aus))
	for i := range clock {
		clock[i] = i * 17
	}
	video := muxWriteVideo(t, aus, clock)
	if len(video) != len(aus) {
		t.Fatalf("PES count = %d, want %d", len(video), len(aus))
	}
	for i, pes := range video {
		if pes.hasDTS {
			t.Fatalf("PES %d carries a split PTS/DTS pair on a reorder-free stream", i)
		}
		if pes.es[len(pes.es)-1] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if i > 0 && pes.pts <= video[i-1].pts {
			t.Fatalf("PTS not strictly increasing at %d", i)
		}
	}
}

func TestWriteVideoStandaloneParameterSetsSurviveAlignment(t *testing.T) {
	// A live join where SPS/PPS arrive as their own AU, then pre-keyframe
	// P-frames, then the IDR (which does NOT repeat the parameter sets).
	// The parameter sets must reach the output ahead of the IDR.
	var aus [][]byte
	var paramsAU []byte
	paramsAU = append(paramsAU, 0x00, 0x00, 0x00, 0x01)
	paramsAU = append(paramsAU, tSPS...)
	paramsAU = append(paramsAU, 0x00, 0x00, 0x00, 0x01)
	paramsAU = append(paramsAU, tPPS...)
	aus = append(aus, paramsAU)
	aus = append(aus, h264SliceAU(false, true, 1, 2, 0xB1, false)) // pre-IDR P: dropped
	aus = append(aus, h264SliceAU(false, true, 2, 4, 0xB2, false)) // pre-IDR P: dropped
	aus = append(aus, h264SliceAU(true, true, 0, 0, 0xC0, false))  // IDR, no in-band params
	aus = append(aus, h264SliceAU(false, true, 1, 2, 0xC1, false))
	clock := []int{0, 17, 34, 51, 68}
	video := muxWriteVideo(t, aus, clock)
	if len(video) != 3 {
		t.Fatalf("PES count = %d, want 3 (params + IDR + P)", len(video))
	}
	if !bytes.Contains(video[0].es, tSPS) || !bytes.Contains(video[0].es, tPPS) {
		t.Fatal("first output PES does not carry the parameter sets")
	}
	if video[1].es[len(video[1].es)-1] != 0xC0 {
		t.Fatal("IDR does not follow the parameter sets")
	}
	for i := 1; i < len(video); i++ {
		if video[i].dts <= video[i-1].dts {
			t.Fatalf("DTS not strictly increasing at %d", i)
		}
	}
}
