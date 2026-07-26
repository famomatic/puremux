package preprocessor

import (
	"slices"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// DTS synthesis bounds. These are deliberately constants, not Config knobs:
// they encode codec-level facts (H.264/HEVC DPB reordering limits), not
// deployment tuning.
const (
	// dtsProbeWindow is the maximum number of frames held at stream start to
	// measure the reorder depth of a B-frame stream. One mini-GOP (typically
	// <= 4 frames) reveals the full pyramid depth, so 8 leaves margin.
	dtsProbeWindow = 8
	// dtsProbeMonoExit ends the probe early: if the first dtsProbeMonoExit
	// frames arrive with non-decreasing PTS the stream is assumed
	// reorder-free (depth 0, DTS == PTS) and held frames are released.
	dtsProbeMonoExit = 4
	// dtsMaxReorderDepth caps the synthesized decode delay at the H.264
	// maximum DPB reorder depth.
	dtsMaxReorderDepth = 16
)

// DTSSynthesizer derives a valid decode timeline for a video stream delivered
// in DECODE order but stamped only with PRESENTATION timestamps (PTS). With
// B-frame reordering such PTS are non-monotonic in decode order, so using
// them as DTS produces an invalid stream; the synthesizer computes a DTS per
// frame such that:
//
//   - frame (payload) order is never changed: decode order in = decode order out;
//   - synthesized DTS never regresses, and in reorder mode advances by at
//     least the configured MinMonotonicStep;
//   - DTS[n] <= PTS[n] for every frame (except a bounded, self-healing
//     transient when the stream's reorder depth grows mid-stream, see below).
//
// Principle: for a stream with decoder reorder depth D, a valid decode
// timeline is the PTS sequence sorted ascending and delayed by D frames
// (DTS[n] = sortedPTS[n-D]); the first D frames extrapolate backwards from
// the earliest PTS. The synthesizer measures D from the stream itself:
//
//   - Probe phase: the first frames are held while the observed reorder depth
//     (max count of earlier-decoded frames with a larger PTS) is measured.
//     If the first dtsProbeMonoExit frames are non-decreasing the stream is
//     declared reorder-free and the synthesizer becomes a passthrough
//     (DTS == PTS, zero added latency, duplicate timestamps left for the
//     Enforcer's nudge exactly as with unsynthesized input). Otherwise the
//     probe runs to dtsProbeWindow frames and locks delay = depth+1 (the +1
//     absorbs one pyramid level appearing only after the probe).
//   - Steady phase: every frame is emitted immediately on arrival (no added
//     latency); its DTS is the smallest not-yet-consumed PTS, which lags the
//     newest arrival by `delay` frames. If a deeper reorder than `delay`
//     appears mid-stream the delay grows adaptively; the frames bridging the
//     growth get minimal-step DTS nudges that may transiently exceed their
//     PTS by roughly one frame interval before the timeline re-converges.
//
// Added latency: at most dtsProbeWindow frames once at stream start
// (dtsProbeMonoExit-1 frames when no reordering is observed); zero afterwards.
//
// The synthesizer NEVER writes to a file and never inspects payload bytes
// (ARCHITECTURE.md sections 4 and 5.B). It is not safe for concurrent use.
type DTSSynthesizer struct {
	step      time.Duration // minimum DTS advance in reorder mode
	probing   bool
	held      []*core.Packet  // decode-order FIFO, only during the probe
	depth     int             // max observed reorder depth
	delay     int             // active decode delay D; never shrinks
	pending   ptsMinHeap      // PTS values not yet consumed as DTS (steady: len == delay)
	recent    []time.Duration // ring of the last dtsMaxReorderDepth PTS values
	recentPos int
	lastDTS   time.Duration
	emitted   bool
}

// NewDTSSynthesizer builds a synthesizer honoring cfg.MinMonotonicStep (the
// same clock-granularity contract as the Enforcer).
func NewDTSSynthesizer(cfg Config) *DTSSynthesizer {
	step := time.Duration(cfg.MinMonotonicStep)
	if step <= 0 {
		step = 1
	}
	return &DTSSynthesizer{step: step, probing: true}
}

// Process takes one decode-order frame whose PTS is set (DTS ignored) and
// emits zero or more frames with synthesized DTS, always in decode order.
// Emitted packets are the caller's to release; held packets are retained
// until a later Process call or Flush.
func (s *DTSSynthesizer) Process(p *core.Packet, emit func(*core.Packet)) {
	if p == nil {
		return
	}
	if s.probing {
		s.probe(p, emit)
		return
	}
	s.steady(p, emit)
}

// Flush releases any frames still held by the startup probe (a stream shorter
// than the probe window). The steady phase holds no frames, so Flush after
// the probe is a no-op.
func (s *DTSSynthesizer) Flush(emit func(*core.Packet)) {
	if !s.probing {
		return
	}
	// End of stream: the measured depth is exact, no margin needed.
	s.finishProbe(min(s.depth, dtsMaxReorderDepth), emit)
}

// probe holds the frame and measures reorder depth until an exit condition.
func (s *DTSSynthesizer) probe(p *core.Packet, emit func(*core.Packet)) {
	if n := s.countLargerPTS(s.heldPTS(), p.PTS); n > s.depth {
		s.depth = n
	}
	s.held = append(s.held, p)
	if s.depth == 0 && len(s.held) >= dtsProbeMonoExit {
		s.finishProbe(0, emit)
		return
	}
	if len(s.held) >= dtsProbeWindow {
		s.finishProbe(min(s.depth+1, dtsMaxReorderDepth), emit)
	}
}

// finishProbe locks the decode delay, assigns DTS to every held frame from
// the sorted-and-delayed PTS timeline, and switches to the steady phase.
func (s *DTSSynthesizer) finishProbe(delay int, emit func(*core.Packet)) {
	s.probing = false
	s.delay = delay
	if len(s.held) == 0 {
		return
	}
	sorted := make([]time.Duration, len(s.held))
	for i, q := range s.held {
		sorted[i] = q.PTS
	}
	slices.Sort(sorted)
	delta := typicalFrameDelta(sorted)
	for k, q := range s.held {
		var cand time.Duration
		if k < delay {
			// Extrapolate the pre-roll below the earliest presentation time,
			// clamped at zero so containers doing unsigned millisecond math
			// (WebM cluster timecodes) never see a negative origin.
			cand = max(sorted[0]-time.Duration(delay-k)*delta, 0)
		} else {
			cand = sorted[k-delay]
		}
		s.pushRecent(q.PTS)
		s.emitWithDTS(q, cand, emit)
	}
	// The top `delay` presentation times were not consumed; they seed the
	// steady-phase pending set. An ascending slice is already a valid min-heap.
	s.pending = append(s.pending[:0], sorted[len(sorted)-delay:]...)
	for i := range s.held {
		s.held[i] = nil
	}
	s.held = s.held[:0]
}

// steady emits the frame immediately with the next delayed-sorted-PTS value.
func (s *DTSSynthesizer) steady(p *core.Packet, emit func(*core.Packet)) {
	if n := s.countLargerPTS(s.recent, p.PTS); n > s.delay {
		s.delay = min(n, dtsMaxReorderDepth)
	}
	s.pushRecent(p.PTS)
	s.pending.push(p.PTS)
	var cand time.Duration
	if len(s.pending) > s.delay {
		cand = s.pending.pop()
	} else {
		// The delay just grew: keep this presentation time pending and bridge
		// with a minimal nudge (the documented mid-stream-growth transient).
		cand = s.lastDTS + s.step
	}
	s.emitWithDTS(p, cand, emit)
}

// emitWithDTS stamps the frame and emits it. In reorder mode (delay > 0) the
// synthesized timeline is kept strictly monotonic by MinMonotonicStep nudges
// that move ONLY the DTS — the caller's PTS is never altered. In passthrough
// mode (delay 0) the candidate is the frame's own PTS and equal-timestamp
// runs are deliberately left intact so the downstream Enforcer applies its
// usual duplicate-timestamp repair, identical to the unsynthesized path.
func (s *DTSSynthesizer) emitWithDTS(p *core.Packet, dts time.Duration, emit func(*core.Packet)) {
	if s.delay > 0 && s.emitted && dts <= s.lastDTS {
		dts = s.lastDTS + s.step
	}
	p.DTS = dts
	s.lastDTS = dts
	s.emitted = true
	emit(p)
}

// heldPTS returns the presentation times of the held probe frames. The probe
// holds at most dtsProbeWindow frames, so the scan is O(8).
func (s *DTSSynthesizer) heldPTS() []time.Duration {
	pts := make([]time.Duration, len(s.held))
	for i, q := range s.held {
		pts[i] = q.PTS
	}
	return pts
}

// countLargerPTS counts values strictly greater than pts: the number of
// earlier-decoded frames this frame presents before, i.e. its reorder depth.
func (s *DTSSynthesizer) countLargerPTS(window []time.Duration, pts time.Duration) int {
	n := 0
	for _, q := range window {
		if q > pts {
			n++
		}
	}
	return n
}

// pushRecent records pts in the bounded decode-order ring used for
// mid-stream reorder-depth growth detection.
func (s *DTSSynthesizer) pushRecent(pts time.Duration) {
	if len(s.recent) < dtsMaxReorderDepth {
		s.recent = append(s.recent, pts)
		return
	}
	s.recent[s.recentPos] = pts
	s.recentPos = (s.recentPos + 1) % dtsMaxReorderDepth
}

// typicalFrameDelta estimates the frame interval as the median of the
// positive gaps in the ascending PTS slice. Returns 0 when every held frame
// shares one timestamp (a duplicate burst); extrapolation then degenerates
// to minimal-step nudges, which is the best available spacing.
func typicalFrameDelta(sorted []time.Duration) time.Duration {
	diffs := make([]time.Duration, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > 0 {
			diffs = append(diffs, d)
		}
	}
	if len(diffs) == 0 {
		return 0
	}
	slices.Sort(diffs)
	return diffs[len(diffs)/2]
}

// ptsMinHeap is a hand-rolled binary min-heap of presentation times (a
// container/heap interface would force per-value allocations for no gain at
// these sizes: len == decode delay <= dtsMaxReorderDepth).
type ptsMinHeap []time.Duration

func (h *ptsMinHeap) push(v time.Duration) {
	*h = append(*h, v)
	a := *h
	for i := len(a) - 1; i > 0; {
		parent := (i - 1) / 2
		if a[parent] <= a[i] {
			break
		}
		a[parent], a[i] = a[i], a[parent]
		i = parent
	}
}

func (h *ptsMinHeap) pop() time.Duration {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	*h = a
	for i := 0; ; {
		l, r := 2*i+1, 2*i+2
		smallest := i
		if l < len(a) && a[l] < a[smallest] {
			smallest = l
		}
		if r < len(a) && a[r] < a[smallest] {
			smallest = r
		}
		if smallest == i {
			break
		}
		a[i], a[smallest] = a[smallest], a[i]
		i = smallest
	}
	return top
}
