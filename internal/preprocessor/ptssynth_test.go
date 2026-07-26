package preprocessor

import (
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// pocAU is one decode-order test AU for the presentation synthesizer.
type pocAU struct {
	tMs   int   // decode clock (PTS == DTS == t on input)
	order int64 // POC order (ignored when pic == false)
	pic   bool
}

// ipbbPOCStream builds `groups` decode-order mini-GOPs of the x264 IPBB
// shape on a 60fps-ish 17ms clock: I(order 0), then per group an anchor
// P(+6) followed by B(+2, ref) and b(+4, non-ref) that display between the
// anchors.
func ipbbPOCStream(groups int) []pocAU {
	aus := []pocAU{{tMs: 0, order: 0, pic: true}}
	t := 17
	base := int64(0)
	for range groups {
		aus = append(aus,
			pocAU{tMs: t, order: base + 6, pic: true},
			pocAU{tMs: t + 17, order: base + 2, pic: true},
			pocAU{tMs: t + 34, order: base + 4, pic: true},
		)
		t += 51
		base += 6
	}
	return aus
}

// feedPTSSynth runs AUs through a synthesizer and returns emitted packets
// (payload byte = decode index) plus per-call emission counts.
func feedPTSSynth(t *testing.T, s *PresentationSynthesizer, aus []pocAU) (out []*core.Packet, perCall []int) {
	t.Helper()
	for i, au := range aus {
		p := core.AcquirePacket()
		p.Data = append(p.Data[:0], byte(i))
		p.PTS = time.Duration(au.tMs) * time.Millisecond
		p.DTS = p.PTS
		before := len(out)
		s.Process(p, core.POCInfo{Order: au.order, Epoch: 1}, au.pic, func(q *core.Packet) {
			out = append(out, q)
		})
		perCall = append(perCall, len(out)-before)
	}
	s.Flush(func(q *core.Packet) { out = append(out, q) })
	return out, perCall
}

// checkPTSSynthInvariants asserts decode order and DTS untouched, PTS >= DTS.
func checkPTSSynthInvariants(t *testing.T, out []*core.Packet, aus []pocAU) {
	t.Helper()
	if len(out) != len(aus) {
		t.Fatalf("emitted %d packets, want %d", len(out), len(aus))
	}
	for i, p := range out {
		if p.Data[0] != byte(i) {
			t.Fatalf("decode order broken at %d: payload tag %d", i, p.Data[0])
		}
		if want := time.Duration(aus[i].tMs) * time.Millisecond; p.DTS != want {
			t.Fatalf("DTS modified at %d: got %v want %v", i, p.DTS, want)
		}
		if p.PTS < p.DTS {
			t.Fatalf("PTS < DTS at %d: pts=%v dts=%v", i, p.PTS, p.DTS)
		}
	}
}

// displayPTS returns the emitted pictures' PTS sorted by display order.
func displayPTS(out []*core.Packet, aus []pocAU) []time.Duration {
	type de struct {
		order int64
		pts   time.Duration
	}
	var des []de
	for i, au := range aus {
		if au.pic {
			des = append(des, de{order: au.order, pts: out[i].PTS})
		}
	}
	for i := 1; i < len(des); i++ {
		for j := i; j > 0 && des[j].order < des[j-1].order; j-- {
			des[j], des[j-1] = des[j-1], des[j]
		}
	}
	pts := make([]time.Duration, len(des))
	for i, d := range des {
		pts[i] = d.pts
	}
	return pts
}

func TestPTSSynthIPBBDisplayTimeline(t *testing.T) {
	aus := ipbbPOCStream(10)
	out, _ := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	// The display-order PTS sequence must be strictly increasing and step by
	// the decode-clock frame interval (17ms) once past the first slot.
	disp := displayPTS(out, aus)
	for i := 1; i < len(disp); i++ {
		if disp[i] <= disp[i-1] {
			t.Fatalf("display PTS not strictly increasing at display index %d: %v <= %v",
				i, disp[i], disp[i-1])
		}
	}
	// Exact expectation: reorder depth 1 (P anchors displaced by 2 B-slots,
	// B displaced by -1) ⇒ delay 1: picture with display rank d gets decode
	// slot d+1: 17, 34, 51, 68, ... except the extrapolated tail.
	for i := 0; i < len(disp)-2; i++ {
		want := time.Duration(17*(i+1)) * time.Millisecond
		if disp[i] != want {
			t.Fatalf("display slot %d = %v, want %v", i, disp[i], want)
		}
	}
}

func TestPTSSynthMonotonicPassthrough(t *testing.T) {
	// No display inversion: after the probe window the stage must be a 1:1
	// zero-latency passthrough with PTS untouched (== DTS).
	var aus []pocAU
	for i := range 20 {
		aus = append(aus, pocAU{tMs: i * 17, order: int64(2 * i), pic: true})
	}
	out, perCall := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	for i, p := range out {
		if p.PTS != p.DTS {
			t.Fatalf("passthrough altered PTS at %d: %v vs %v", i, p.PTS, p.DTS)
		}
	}
	if perCall[ptsProbeWindow-1] != ptsProbeWindow {
		t.Fatalf("probe exit emitted %d, want %d", perCall[ptsProbeWindow-1], ptsProbeWindow)
	}
	for i := ptsProbeWindow; i < len(perCall); i++ {
		if perCall[i] != 1 {
			t.Fatalf("passthrough call %d emitted %d, want 1", i, perCall[i])
		}
	}
}

func TestPTSSynthZeroSteadyLatencyInReorderMode(t *testing.T) {
	// Reorder mode delays by the lookahead window but must then emit exactly
	// one picture per arriving picture (bounded, constant latency).
	aus := ipbbPOCStream(12)
	out, perCall := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	for i := ptsProbeWindow + ptsMaxReorder; i < len(perCall); i++ {
		if perCall[i] != 1 {
			t.Fatalf("steady call %d emitted %d packets, want 1", i, perCall[i])
		}
	}
}

func TestPTSSynthNonPictureAUsFlowInOrder(t *testing.T) {
	// Parameter-set/SEI-only AUs must ride along in decode order with
	// PTS == DTS, in both probe and steady phases.
	aus := ipbbPOCStream(6)
	// Inject non-picture AUs at the front (in-band SPS/PPS before the IDR)
	// and mid-stream.
	aus = append([]pocAU{{tMs: 0, pic: false}}, aus...)
	mid := len(aus) / 2
	aus = append(aus[:mid], append([]pocAU{{tMs: aus[mid-1].tMs, pic: false}}, aus[mid:]...)...)
	out, _ := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	for i, au := range aus {
		if !au.pic && out[i].PTS != out[i].DTS {
			t.Fatalf("non-picture AU %d PTS altered", i)
		}
	}
}

func TestPTSSynthShortStreamFlush(t *testing.T) {
	// Fewer pictures than the probe window: Flush must emit everything with
	// valid timestamps (reorder detected inside the probe window).
	aus := []pocAU{
		{tMs: 0, order: 0, pic: true},
		{tMs: 17, order: 6, pic: true},
		{tMs: 34, order: 2, pic: true},
		{tMs: 51, order: 4, pic: true},
	}
	out, perCall := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	for _, n := range perCall {
		if n != 0 {
			t.Fatalf("probe leaked frames early: %v", perCall)
		}
	}
	disp := displayPTS(out, aus)
	for i := 1; i < len(disp); i++ {
		if disp[i] <= disp[i-1] {
			t.Fatalf("display PTS not increasing at %d", i)
		}
	}
}

func TestPTSSynthDeepPyramid(t *testing.T) {
	// x264 b-pyramid shape: I(0), then per group anchor P(+8) followed by
	// B(+4), b(+2), b(+6) — depth 2. Steady display timeline must be strictly
	// increasing with 17ms steps.
	aus := []pocAU{{tMs: 0, order: 0, pic: true}}
	tt := 17
	base := int64(0)
	for range 10 {
		aus = append(aus,
			pocAU{tMs: tt, order: base + 8, pic: true},
			pocAU{tMs: tt + 17, order: base + 4, pic: true},
			pocAU{tMs: tt + 34, order: base + 2, pic: true},
			pocAU{tMs: tt + 51, order: base + 6, pic: true},
		)
		tt += 68
		base += 8
	}
	out, _ := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	disp := displayPTS(out, aus)
	for i := 1; i < len(disp); i++ {
		if disp[i] <= disp[i-1] {
			t.Fatalf("display PTS not strictly increasing at %d: %v <= %v", i, disp[i], disp[i-1])
		}
	}
	for i := 0; i < len(disp)-3; i++ {
		want := time.Duration(17*(i+2)) * time.Millisecond // delay 2
		if disp[i] != want {
			t.Fatalf("display slot %d = %v, want %v", i, disp[i], want)
		}
	}
}

func TestPTSSynthJitteryStartupClock(t *testing.T) {
	// The live pathology: the first AUs arrive with duplicated/out-of-order
	// decode-clock stamps while the POC is clean. PTS >= DTS must hold for
	// every frame and the steady tail must be a clean increasing display
	// timeline.
	aus := ipbbPOCStream(12)
	// Scramble the first 8 decode clocks: duplicates + inversions.
	jitter := []int{50, 0, 50, 34, 17, 100, 84, 67}
	for i := range jitter {
		aus[i].tMs = jitter[i]
	}
	out, _ := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	if len(out) != len(aus) {
		t.Fatalf("emitted %d, want %d", len(out), len(aus))
	}
	for i, p := range out {
		if p.Data[0] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if p.PTS < p.DTS {
			t.Fatalf("PTS < DTS at %d: %v < %v", i, p.PTS, p.DTS)
		}
	}
	disp := displayPTS(out, aus)
	for i := len(disp) - 12; i < len(disp); i++ {
		if disp[i] <= disp[i-1] {
			t.Fatalf("steady display tail not increasing at %d", i)
		}
	}
}

func TestPTSSynthMidStreamEngagement(t *testing.T) {
	// Monotonic prefix long enough to pass the probe, then B-frames appear.
	// The engagement transient is bounded: after it, display PTS increases
	// strictly and PTS >= DTS everywhere.
	var aus []pocAU
	for i := range 12 {
		aus = append(aus, pocAU{tMs: i * 17, order: int64(2 * i), pic: true})
	}
	tt := 12 * 17
	base := int64(2 * 11)
	for range 8 {
		aus = append(aus,
			pocAU{tMs: tt, order: base + 6, pic: true},
			pocAU{tMs: tt + 17, order: base + 2, pic: true},
			pocAU{tMs: tt + 34, order: base + 4, pic: true},
		)
		tt += 51
		base += 6
	}
	out, _ := feedPTSSynth(t, NewPresentationSynthesizer(DefaultConfig()), aus)
	defer releaseAll(out)
	checkPTSSynthInvariants(t, out, aus)
	disp := displayPTS(out, aus)
	for i := len(disp) - 9; i < len(disp); i++ {
		if disp[i] <= disp[i-1] {
			t.Fatalf("post-engagement display tail not increasing at %d", i)
		}
	}
}
