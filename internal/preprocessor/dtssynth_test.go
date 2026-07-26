package preprocessor

import (
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

func synthCfg() Config {
	cfg := DefaultConfig()
	cfg.MinMonotonicStep = uint64(time.Millisecond)
	return cfg
}

func synthPkt(pts time.Duration, tag byte) *core.Packet {
	p := core.AcquirePacket()
	p.Data = append(p.Data[:0], tag)
	p.PTS = pts
	p.DTS = pts
	return p
}

// feedSynth pushes the decode-order PTS sequence through a synthesizer and
// returns the emitted packets plus the per-Process emission counts.
func feedSynth(t *testing.T, s *DTSSynthesizer, ptsMs []int) (out []*core.Packet, perCall []int) {
	t.Helper()
	for i, ms := range ptsMs {
		before := len(out)
		s.Process(synthPkt(time.Duration(ms)*time.Millisecond, byte(i)), func(p *core.Packet) {
			out = append(out, p)
		})
		perCall = append(perCall, len(out)-before)
	}
	s.Flush(func(p *core.Packet) { out = append(out, p) })
	return out, perCall
}

// checkSynthInvariants asserts decode order preserved, PTS unmodified, DTS
// strictly monotonic, and DTS <= PTS on every frame.
func checkSynthInvariants(t *testing.T, out []*core.Packet, ptsMs []int) {
	t.Helper()
	if len(out) != len(ptsMs) {
		t.Fatalf("emitted %d packets, want %d", len(out), len(ptsMs))
	}
	for i, p := range out {
		if p.Data[0] != byte(i) {
			t.Fatalf("decode order broken at %d: payload tag %d", i, p.Data[0])
		}
		if want := time.Duration(ptsMs[i]) * time.Millisecond; p.PTS != want {
			t.Fatalf("PTS modified at %d: got %v want %v", i, p.PTS, want)
		}
		if p.DTS > p.PTS {
			t.Fatalf("DTS > PTS at %d: dts=%v pts=%v", i, p.DTS, p.PTS)
		}
		if i > 0 && p.DTS <= out[i-1].DTS {
			t.Fatalf("DTS not strictly increasing at %d: %v <= %v", i, p.DTS, out[i-1].DTS)
		}
	}
}

func releaseAll(out []*core.Packet) {
	for _, p := range out {
		core.ReleasePacket(p)
	}
}

func TestDTSSynthIBBPFlush(t *testing.T) {
	// One mini-GOP in decode order I,P,B,B with presentation I < B1 < B2 < P,
	// shorter than the probe window so Close-time Flush must drain it.
	pts := []int{0, 30, 10, 20}
	out, _ := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
	// Exact expected timeline: depth 1, sorted [0,10,20,30], delta 10ms.
	// f0 extrapolates to -10ms -> clamped 0; f1 takes sorted[0]=0 -> nudged
	// +1ms; f2/f3 take sorted[1]/sorted[2].
	want := []time.Duration{0, time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	for i, p := range out {
		if p.DTS != want[i] {
			t.Fatalf("DTS[%d] = %v, want %v", i, p.DTS, want[i])
		}
	}
}

func TestDTSSynthSteadyStateZeroLatency(t *testing.T) {
	// Continuous IPBB cadence: I(0) P(+30) B B, next P(+30) B B ... in decode
	// order. After the probe window every frame must be emitted by the very
	// Process call that delivered it (zero steady-state latency).
	var pts []int
	base := 0
	pts = append(pts, 0)
	for range 8 {
		p := base + 30
		pts = append(pts, p, base+10, base+20)
		base = p
	}
	out, perCall := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
	for i := dtsProbeWindow; i < len(perCall); i++ {
		if perCall[i] != 1 {
			t.Fatalf("steady call %d emitted %d packets, want 1", i, perCall[i])
		}
	}
}

func TestDTSSynthMonotonicPassthrough(t *testing.T) {
	// No reordering: probe must exit after a full dtsProbeWindow of monotonic
	// frames, and every frame must come out with DTS exactly equal to its PTS.
	pts := []int{0, 20, 40, 60, 80, 100, 120, 140, 160, 180, 200, 220}
	out, perCall := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
	for i, p := range out {
		if p.DTS != p.PTS {
			t.Fatalf("DTS != PTS at %d: %v vs %v", i, p.DTS, p.PTS)
		}
	}
	// Probe releases everything once the window fills...
	if perCall[dtsProbeWindow-1] != dtsProbeWindow {
		t.Fatalf("probe exit emitted %d, want %d", perCall[dtsProbeWindow-1], dtsProbeWindow)
	}
	// ...and is a 1:1 passthrough afterwards.
	for i := dtsProbeWindow; i < len(perCall); i++ {
		if perCall[i] != 1 {
			t.Fatalf("passthrough call %d emitted %d packets, want 1", i, perCall[i])
		}
	}
}

func TestDTSSynthDuplicateTimestampsLeftForEnforcer(t *testing.T) {
	// A live backfill burst: several frames sharing one timestamp, no
	// reordering. The synthesizer must NOT resolve the duplicates itself
	// (DTS stays == PTS) so the Enforcer applies its usual PTS+DTS nudge and
	// the output matches the unsynthesized WriteVideo path byte-for-byte.
	pts := []int{500, 500, 500, 500, 520, 540, 560}
	out, _ := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	if len(out) != len(pts) {
		t.Fatalf("emitted %d, want %d", len(out), len(pts))
	}
	for i, p := range out {
		if p.Data[0] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if p.DTS != p.PTS {
			t.Fatalf("duplicate run altered at %d: dts=%v pts=%v", i, p.DTS, p.PTS)
		}
	}
}

func TestDTSSynthShortStreamFlush(t *testing.T) {
	// Fewer frames than any probe exit: Flush must emit them all (DTS==PTS,
	// measured depth 0).
	pts := []int{0, 30}
	out, perCall := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
	if perCall[0] != 0 || perCall[1] != 0 {
		t.Fatalf("probe leaked frames early: %v", perCall)
	}
	for i, p := range out {
		if p.DTS != p.PTS {
			t.Fatalf("DTS != PTS at %d", i)
		}
	}
}

// steadyBFramePTS returns `groups` mini-GOPs of a 60fps decode-order pattern
// with reorder depth 2 (two anchors then the B-frame presenting between
// them): for base b at 50ms spacing, b+34, b+50, b+17.
func steadyBFramePTS(startMs, groups int) []int {
	var pts []int
	for g := range groups {
		b := startMs + 50*g
		pts = append(pts, b+34, b+50, b+17)
	}
	return pts
}

func TestDTSSynthScrambledStartupDeterministic(t *testing.T) {
	// The v0.0.8 regression: a live session that starts with a backfill burst
	// delivered out of order with a duplicated timestamp, then settles into a
	// clean B-frame GOP. The v0.0.7 one-shot probe measured the reorder depth
	// from the scrambled burst, so the synthesized timeline depended on the
	// burst's arrival order (non-deterministic across sessions). Every
	// scramble must now yield strict invariants for EVERY frame, and the
	// decode delay must converge to the steady stream's depth regardless of
	// how badly the burst inflated the startup measurement.
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
		pts := append(slices.Clone(burst), steadyBFramePTS(14000, 40)...)
		s := NewDTSSynthesizer(synthCfg())
		out, _ := feedSynth(t, s, pts)
		checkSynthInvariants(t, out, pts)
		// Decay must have walked any burst-inflated delay back down to the
		// steady pattern's depth (2) + 1 margin.
		if s.delay > 3 {
			releaseAll(out)
			t.Fatalf("seed %d: delay %d did not decay to the steady depth", seed, s.delay)
		}
		releaseAll(out)
	}
}

func TestDTSSynthDuplicateBurstThenBFrames(t *testing.T) {
	// The production jittery-startup shape: a backfill burst of frames all
	// sharing one timestamp (no ordering evidence at all), then the clean
	// B-frame pattern. The v0.0.7 probe declared this "monotonic" after 4
	// duplicates and locked DTS==PTS passthrough for the whole stream; the
	// probe must instead keep holding until real ordering evidence arrives
	// and then emit a fully valid timeline — no DTS>PTS transient at all.
	burst := []int{13900, 13900, 13900, 13900, 13900, 13900, 13900, 13900}
	pts := append(slices.Clone(burst), steadyBFramePTS(14000, 20)...)
	out, _ := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
}

func TestDTSSynthDuplicateOnlyStreamStaysPassthrough(t *testing.T) {
	// A stream that never yields ordering evidence (every frame one
	// timestamp) must exit the probe at the hard cap as a passthrough, with
	// bounded latency, leaving the duplicates for the Enforcer.
	pts := make([]int, dtsProbeMaxExtend+8)
	for i := range pts {
		pts[i] = 700
	}
	out, perCall := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	if len(out) != len(pts) {
		t.Fatalf("emitted %d, want %d", len(out), len(pts))
	}
	for i, p := range out {
		if p.DTS != p.PTS {
			t.Fatalf("duplicate run altered at %d: dts=%v pts=%v", i, p.DTS, p.PTS)
		}
	}
	if perCall[dtsProbeMaxExtend-1] != dtsProbeMaxExtend {
		t.Fatalf("probe cap exit emitted %d, want %d", perCall[dtsProbeMaxExtend-1], dtsProbeMaxExtend)
	}
}

func TestDTSSynthDelayDecaysAfterInflatedStart(t *testing.T) {
	// A fully reversed startup window inflates the measured depth to the
	// window maximum. The steady-phase decay must walk the delay back to the
	// observed steady depth instead of locking the inflated lead forever.
	burst := []int{13984, 13967, 13950, 13934, 13917, 13900, 13884, 13867}
	pts := append(slices.Clone(burst), steadyBFramePTS(14000, 40)...)
	s := NewDTSSynthesizer(synthCfg())
	out, _ := feedSynth(t, s, pts)
	defer releaseAll(out)
	checkSynthInvariants(t, out, pts)
	if s.delay > 3 {
		t.Fatalf("delay %d did not decay to the steady depth", s.delay)
	}
}

func TestDTSSynthMidStreamDepthGrowth(t *testing.T) {
	// The stream starts without B-frames (probe exits at depth 0), then
	// introduces reordering. The synthesizer must stay strictly monotonic and
	// re-converge to DTS <= PTS after a bounded transient.
	pts := []int{0, 20, 40, 60, 80, 100, // monotonic prefix, probe exits depth 0
		160, 120, 140, // first B-frames: growth transient
		220, 180, 200, 280, 240, 260}
	out, _ := feedSynth(t, NewDTSSynthesizer(synthCfg()), pts)
	defer releaseAll(out)
	if len(out) != len(pts) {
		t.Fatalf("emitted %d, want %d", len(out), len(pts))
	}
	violations := 0
	for i, p := range out {
		if p.Data[0] != byte(i) {
			t.Fatalf("decode order broken at %d", i)
		}
		if i > 0 && p.DTS <= out[i-1].DTS {
			t.Fatalf("DTS not strictly increasing at %d", i)
		}
		if p.DTS > p.PTS {
			violations++
		}
	}
	// The growth transient is bounded and must have healed by the tail.
	if violations > 3 {
		t.Fatalf("too many DTS>PTS transients: %d", violations)
	}
	for i := len(out) - 4; i < len(out); i++ {
		if out[i].DTS > out[i].PTS {
			t.Fatalf("timeline did not re-converge: DTS>PTS at %d", i)
		}
	}
}
