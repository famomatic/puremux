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
	// dtsProbeWindow is the number of frames held at stream start to measure
	// the reorder depth of a B-frame stream. One mini-GOP (typically <= 4
	// frames) reveals the full pyramid depth, so 8 leaves margin; it also
	// covers a startup backfill burst of up to 8 scrambled frames, which the
	// window-based measurement handles because every burst frame is measured
	// against all of its held predecessors (order-tolerant within the window).
	dtsProbeWindow = 8
	// dtsProbeMinDistinct is the minimum number of DISTINCT presentation
	// timestamps the probe must see before declaring a stream reorder-free.
	// A duplicate-stamped backfill burst (every frame sharing one timestamp)
	// carries no ordering evidence at all: counting it as "monotonic" locked
	// the v0.0.7 probe into DTS==PTS passthrough right before the stream's
	// first real B-frames arrived.
	dtsProbeMinDistinct = 4
	// dtsProbeMaxExtend bounds the probe when the window is duplicate-heavy
	// (fewer than dtsProbeMinDistinct distinct timestamps after
	// dtsProbeWindow frames): the probe keeps holding frames until enough
	// distinct timestamps arrive or this hard cap is reached, so startup
	// latency stays bounded even for pathological all-duplicate streams.
	dtsProbeMaxExtend = 32
	// dtsMaxReorderDepth caps the synthesized decode delay at the H.264
	// maximum DPB reorder depth.
	dtsMaxReorderDepth = 16
	// dtsDepthDecayWindow is the number of steady-phase frames over which the
	// observed reorder depth is aggregated before the decode delay is allowed
	// to shrink one step toward it. A scrambled startup burst can inflate the
	// measured depth well past the stream's true steady-state depth; without
	// decay that one-shot over-measurement would lock a permanently excessive
	// DTS lead. The window spans several mini-GOPs so a normal B-pyramid never
	// looks shallower than it is.
	dtsDepthDecayWindow = 16
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
// the earliest PTS. The synthesizer measures D from the stream itself,
// continuously — no measurement is ever locked permanently:
//
//   - Probe phase: the first dtsProbeWindow frames are held while the
//     observed reorder depth (max count of earlier-decoded frames with a
//     larger PTS) is measured across the whole window. Because every frame is
//     compared against all held predecessors, the measurement is
//     order-tolerant within the window: a scrambled startup burst yields the
//     depth needed to cover its own jitter, deterministically, regardless of
//     arrival order. If the full window shows no reordering across at least
//     dtsProbeMinDistinct distinct timestamps the stream is declared
//     reorder-free and the synthesizer becomes a passthrough (DTS == PTS,
//     zero added latency, duplicate timestamps left for the Enforcer's nudge
//     exactly as with unsynthesized input). A duplicate-heavy window
//     (duplicated backfill timestamps carry no ordering evidence) extends the
//     probe up to dtsProbeMaxExtend frames instead of mis-declaring
//     passthrough. Otherwise the probe locks in delay = depth+1 (the +1
//     absorbs one pyramid level appearing only after the probe).
//   - Steady phase: every frame is emitted immediately on arrival (no added
//     latency); its DTS is the smallest not-yet-consumed PTS, which lags the
//     newest arrival by `delay` frames. The reorder depth keeps being
//     measured over a sliding window of recent frames: if a deeper reorder
//     appears the delay grows immediately (the bridging frames get
//     minimal-step DTS nudges that may transiently exceed their PTS by
//     roughly one frame interval before the timeline re-converges); if the
//     observed depth stays below the active delay for dtsDepthDecayWindow
//     consecutive frames the delay shrinks one step (skipping ahead in the
//     sorted-PTS timeline, which preserves monotonicity and DTS <= PTS), so
//     a startup burst can never lock a permanently excessive lead.
//
// Added latency: at most dtsProbeWindow frames once at stream start
// (dtsProbeMaxExtend for a pathological duplicate-only start); zero
// afterwards.
//
// The synthesizer NEVER writes to a file and never inspects payload bytes
// (ARCHITECTURE.md sections 4 and 5.B). It is not safe for concurrent use.
type DTSSynthesizer struct {
	step       time.Duration // minimum DTS advance in reorder mode
	probing    bool
	held       []*core.Packet  // decode-order FIFO, only during the probe
	depth      int             // max observed reorder depth (probe phase)
	distinct   int             // distinct PTS values seen by the probe
	dupMax     int             // longest duplicate-PTS run seen by the probe
	delay      int             // active decode delay D; adapts both directions
	pending    ptsMinHeap      // PTS values not yet consumed as DTS (steady: len == delay)
	recent     []time.Duration // ring of the last dtsMaxReorderDepth PTS values
	recentPos  int
	decayMax   int // max reorder depth observed in the current decay window
	decayCount int // frames accumulated in the current decay window
	lastDTS    time.Duration
	emitted    bool
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
	// End of stream: the measured depth is exact, no margin needed. Reordered
	// duplicate runs still need delay >= run length (see probe).
	delay := s.depth
	if delay > 0 {
		delay = max(delay, s.dupMax)
	}
	s.finishProbe(min(delay, dtsMaxReorderDepth), emit)
}

// probe holds the frame and measures reorder depth until an exit condition.
func (s *DTSSynthesizer) probe(p *core.Packet, emit func(*core.Packet)) {
	larger, equal := 0, 0
	for _, q := range s.held {
		switch {
		case q.PTS > p.PTS:
			larger++
		case q.PTS == p.PTS:
			equal++
		}
	}
	if larger > s.depth {
		s.depth = larger
	}
	if equal == 0 {
		s.distinct++
	}
	if equal+1 > s.dupMax {
		s.dupMax = equal + 1
	}
	s.held = append(s.held, p)
	if len(s.held) < dtsProbeWindow {
		return
	}
	switch {
	case s.depth > 0:
		// Reordering observed: the full window's measurement covers every held
		// frame's displacement, so depth+1 is a safe decode delay for both the
		// burst and (with margin) the steady stream behind it. A duplicate run
		// needs at least its own length as delay so every duplicate's DTS can
		// sit below the shared PTS (strict monotonicity forces them apart).
		s.finishProbe(min(max(s.depth+1, s.dupMax), dtsMaxReorderDepth), emit)
	case s.distinct >= dtsProbeMinDistinct:
		// A full window of genuinely monotonic timestamps: reorder-free.
		s.finishProbe(0, emit)
	case len(s.held) >= dtsProbeMaxExtend:
		// Duplicate-only start with no ordering evidence at the hard cap:
		// give up and pass through (the Enforcer handles duplicate repair).
		s.finishProbe(0, emit)
	}
	// Otherwise: duplicate-heavy window, keep probing for ordering evidence.
}

// finishProbe locks the initial decode delay, assigns DTS to every held frame
// from the sorted-and-delayed PTS timeline, and switches to the steady phase
// (where the delay keeps adapting in both directions).
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

// steady emits the frame immediately with the next delayed-sorted-PTS value,
// growing the decode delay as soon as a deeper reorder is observed and
// shrinking it (one step per dtsDepthDecayWindow frames of evidence) when the
// stream's observed depth stays below it.
func (s *DTSSynthesizer) steady(p *core.Packet, emit func(*core.Packet)) {
	n := s.countLargerPTS(s.recent, p.PTS)
	if n > s.delay {
		s.delay = min(n, dtsMaxReorderDepth)
	}
	s.updateDepthDecay(n)
	s.pushRecent(p.PTS)
	s.pending.push(p.PTS)
	var cand time.Duration
	if len(s.pending) > s.delay {
		cand = s.pending.pop()
		// After a delay shrink the pending set is oversized; consuming the
		// surplus here skips the synthesized timeline ahead in the sorted-PTS
		// order (monotonic, and closer to — never past — the frame's PTS).
		for len(s.pending) > s.delay {
			cand = s.pending.pop()
		}
	} else {
		// The delay just grew: keep this presentation time pending and bridge
		// with a minimal nudge (the documented mid-stream-growth transient).
		cand = s.lastDTS + s.step
	}
	s.emitWithDTS(p, cand, emit)
}

// updateDepthDecay aggregates the per-frame observed reorder depth n and,
// once a full evidence window shows the active delay exceeds the observed
// depth (+1 margin), shrinks the delay one step. Passthrough (delay 0) is
// never re-entered: the floor in reorder mode is delay 1.
func (s *DTSSynthesizer) updateDepthDecay(n int) {
	if n > s.decayMax {
		s.decayMax = n
	}
	s.decayCount++
	if s.decayCount < dtsDepthDecayWindow {
		return
	}
	if target := s.decayMax + 1; s.delay > target {
		s.delay--
	}
	s.decayMax, s.decayCount = 0, 0
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
// steady-phase reorder-depth measurement.
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
