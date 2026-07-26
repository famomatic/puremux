package preprocessor

import (
	"slices"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// Presentation synthesis bounds (codec-level facts, not tuning knobs).
const (
	// ptsProbeWindow is the number of coded pictures held at stream start
	// while the probe decides whether the bitstream reorders its output
	// (any display-order inversion within the window). One mini-GOP reveals
	// B-frames; 8 covers deep pyramids.
	ptsProbeWindow = 8
	// ptsProbeMaxHold bounds the probe by total held access units, so an AU
	// stream that never yields a parseable picture (e.g. an unsupported POC
	// variant) falls back to passthrough with bounded latency.
	ptsProbeMaxHold = 32
	// ptsMaxReorder caps the lookahead window and display delay at the
	// H.264/HEVC DPB reorder maximum.
	ptsMaxReorder = 16
	// ptsRecentEmitted is how many recently emitted pictures are remembered
	// for display-rank computation; bounded by the DPB like ptsMaxReorder.
	ptsRecentEmitted = 16
	// ptsDeltaWindow is how many recent positive decode-clock deltas feed the
	// frame-interval estimate used to extrapolate presentation slots that lie
	// beyond the newest arrived picture.
	ptsDeltaWindow = 9
)

// PresentationSynthesizer derives per-frame PRESENTATION timestamps for video
// delivered in DECODE order on a monotonic decode clock (PTS == DTS == t)
// whose bitstream reorders display via the picture order count — the live
// WriteVideo shape. It is the mirror image of DTSSynthesizer (which derives
// DTS from decode-order PTS): here the decode timeline is trusted and the
// display timeline is synthesized.
//
// Principle: give the picture with display rank d the decode-clock slot
// t[d + D], where D is the stream's reorder depth (the causality delay: a
// frame delivered late cannot present at its capture slot). The rank comes
// from the codec's POC (parsed by the caller via core.PictureOrderParser and
// passed in — this stage never inspects payload bytes, §5.A):
//
//	rank(f) = decodeIndex(f) − a(f) + b(f)
//
// with a(f) = pictures decoded before f that display after it (bounded by
// the DPB, counted over the recently emitted ring) and b(f) = pictures
// decoded after f that display before it (counted over the lookahead
// window, which is what the window exists for). D = max(a−b) observed, so
// PTS ≥ DTS always holds in steady state; it is measured from POC alone,
// so a jittery decode clock at startup cannot corrupt it.
//
//   - Probe phase: the first ptsProbeWindow pictures are held. No display
//     inversion in the window ⇒ passthrough forever (PTS == DTS, zero
//     added latency, byte-identical output). An inversion ⇒ reorder mode.
//   - Reorder mode: a sliding lookahead of W pictures (W adapts to the
//     observed reorder depth, capped at the DPB bound) delays each picture
//     just long enough to see every later-decoded picture that displays
//     before it. Presentation slots beyond the newest arrival (anchors) are
//     extrapolated from the median decode-clock delta.
//   - Non-picture AUs (parameter sets, SEI, unparseable slices) flow through
//     the same FIFO with PTS == DTS, preserving byte order exactly.
//
// The synthesized PTS is clamped to >= DTS per frame, so a scrambled or
// duplicate-stamped startup burst degrades locally instead of corrupting the
// timeline. Frames still held are drained by Flush (Close).
//
// It never writes to a file and is not safe for concurrent use.
type PresentationSynthesizer struct {
	probing bool
	reorder bool
	win     []pentry

	prevArrived core.POCInfo // last arrived picture (probe inversion check)
	prevSet     bool
	sawInvert   bool

	lookahead int // W: pictures of lookahead in reorder mode

	picIdx  int64 // decode index of the next picture to emit
	arrived int64 // pictures arrived (tRing high-water)
	tRing   [64]time.Duration
	lastT   time.Duration
	deltas  []time.Duration
	delay   int64 // D: display delay in frames; grows only

	emitted    []core.POCInfo // ring of recently emitted pictures
	emittedPos int
}

type pentry struct {
	pkt *core.Packet
	poc core.POCInfo
	pic bool
}

// NewPresentationSynthesizer builds a synthesizer. The Config is accepted for
// symmetry with the other preprocessor stages (no knobs are read today).
func NewPresentationSynthesizer(Config) *PresentationSynthesizer {
	return &PresentationSynthesizer{probing: true, lookahead: 3}
}

// Process takes one decode-order access unit with its parsed picture-order
// info (isPicture=false for AUs carrying no parseable coded picture) and
// emits zero or more AUs with synthesized PTS, always in decode order.
func (s *PresentationSynthesizer) Process(p *core.Packet, poc core.POCInfo, isPicture bool, emit func(*core.Packet)) {
	if p == nil {
		return
	}
	if isPicture {
		s.tRing[s.arrived%int64(len(s.tRing))] = p.DTS
		if s.arrived > 0 {
			if d := p.DTS - s.lastT; d > 0 {
				s.deltas = append(s.deltas, d)
				if len(s.deltas) > ptsDeltaWindow {
					s.deltas = s.deltas[1:]
				}
			}
		}
		s.lastT = p.DTS
		s.arrived++
		if s.prevSet && poc.Before(s.prevArrived) {
			s.sawInvert = true
			if !s.probing && !s.reorder {
				// B-frames appeared after the probe declared passthrough:
				// engage reorder mode mid-stream (documented transient — the
				// already-emitted frames kept PTS == DTS).
				s.reorder = true
			}
			if !s.probing && s.reorder {
				s.growIfLate(poc)
			}
		}
		s.prevArrived = poc
		s.prevSet = true
	}
	s.win = append(s.win, pentry{pkt: p, poc: poc, pic: isPicture})

	if s.probing {
		if s.picCount() >= ptsProbeWindow || len(s.win) >= ptsProbeMaxHold {
			s.finishProbe()
		} else {
			return
		}
	}
	s.drain(false, emit)
}

// Flush drains everything still held (probe or lookahead tail).
func (s *PresentationSynthesizer) Flush(emit func(*core.Packet)) {
	if s.probing {
		s.finishProbe()
	}
	s.drain(true, emit)
}

// picCount counts coded pictures currently held.
func (s *PresentationSynthesizer) picCount() int {
	n := 0
	for _, e := range s.win {
		if e.pic {
			n++
		}
	}
	return n
}

// finishProbe decides passthrough vs reorder from the held window.
func (s *PresentationSynthesizer) finishProbe() {
	s.probing = false
	s.reorder = s.sawInvert
	if !s.reorder {
		return
	}
	// Size the lookahead and preset the display delay from the window's own
	// structure, so the very first emitted pictures already use the final
	// delay (no colliding presentation slots while it ramps up): bmax is the
	// deepest "decoded after, displays before" count, dispmax the deepest
	// displacement a(i)−b(i).
	bmax, dispmax := 0, int64(0)
	pics := s.heldPics()
	for i, pi := range pics {
		a, b := 0, 0
		for _, pj := range pics[:i] {
			if pi.Before(pj) {
				a++
			}
		}
		for _, pj := range pics[i+1:] {
			if pj.Before(pi) {
				b++
			}
		}
		if b > bmax {
			bmax = b
		}
		if d := int64(a - b); d > dispmax {
			dispmax = d
		}
	}
	s.lookahead = min(bmax+2, ptsMaxReorder)
	s.delay = min(dispmax, ptsMaxReorder)
}

// heldPics returns the POC of every held picture, in decode order.
func (s *PresentationSynthesizer) heldPics() []core.POCInfo {
	pics := make([]core.POCInfo, 0, len(s.win))
	for _, e := range s.win {
		if e.pic {
			pics = append(pics, e.poc)
		}
	}
	return pics
}

// growIfLate widens the lookahead when a new picture displays before an
// already-emitted one — evidence the current window was too shallow. The
// late picture itself degrades (clamped PTS); future ones are covered.
func (s *PresentationSynthesizer) growIfLate(poc core.POCInfo) {
	late := 0
	for _, e := range s.emitted {
		if poc.Before(e) {
			late++
		}
	}
	if late > 0 {
		s.lookahead = min(s.lookahead+late, ptsMaxReorder)
	}
}

// drain emits window entries per the active mode. Non-picture entries at the
// head always flow. In reorder mode pictures are held back until `lookahead`
// pictures sit behind them (or the stream is flushing).
func (s *PresentationSynthesizer) drain(flush bool, emit func(*core.Packet)) {
	for len(s.win) > 0 {
		head := s.win[0]
		if head.pic && s.reorder {
			if !flush && s.picCount()-1 < s.lookahead {
				return
			}
			s.emitPicture(head, emit)
		} else if head.pic {
			// Passthrough picture: keep rank bookkeeping current so a
			// mid-stream switch to reorder mode starts with real context.
			s.pushEmitted(head.poc)
			s.picIdx++
			emit(head.pkt)
		} else {
			emit(head.pkt)
		}
		s.win[0] = pentry{}
		s.win = s.win[1:]
		if len(s.win) == 0 {
			s.win = nil
		}
	}
}

// emitPicture assigns the head picture its presentation slot and emits it.
func (s *PresentationSynthesizer) emitPicture(head pentry, emit func(*core.Packet)) {
	a := int64(0)
	for _, e := range s.emitted {
		if head.poc.Before(e) {
			a++
		}
	}
	b := int64(0)
	for _, e := range s.win[1:] {
		if e.pic && e.poc.Before(head.poc) {
			b++
		}
	}
	rank := s.picIdx - a + b
	if disp := a - b; disp > s.delay {
		// A deeper reorder than any seen: presentation must lag decode by at
		// least the displacement or PTS would precede DTS.
		s.delay = min(disp, ptsMaxReorder)
	}
	pts := s.slotTime(rank + s.delay)
	// A jittery decode clock (scrambled/duplicated startup burst) can make a
	// slot precede this frame's own decode time; presentation can never do
	// that, so degrade locally rather than corrupt the timeline.
	pts = max(pts, head.pkt.DTS)
	head.pkt.PTS = pts
	s.pushEmitted(head.poc)
	s.picIdx++
	emit(head.pkt)
}

// slotTime maps an absolute decode-clock slot index to a time: a remembered
// arrival when the slot is in range, otherwise an extrapolation from the
// newest arrival using the median observed frame interval.
func (s *PresentationSynthesizer) slotTime(idx int64) time.Duration {
	ringLen := int64(len(s.tRing))
	if idx < s.arrived {
		if idx >= s.arrived-ringLen && idx >= 0 {
			return s.tRing[idx%ringLen]
		}
		return s.lastT // ancient slot (cannot happen with bounded a); degrade
	}
	return s.lastT + time.Duration(idx-(s.arrived-1))*s.typicalDelta()
}

// typicalDelta estimates the decode-clock frame interval as the median of
// recent positive deltas; 0 when nothing positive has been seen yet (the
// PTS >= DTS clamp then takes over).
func (s *PresentationSynthesizer) typicalDelta() time.Duration {
	if len(s.deltas) == 0 {
		return 0
	}
	sorted := slices.Clone(s.deltas)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// pushEmitted records an emitted picture in the bounded ring.
func (s *PresentationSynthesizer) pushEmitted(poc core.POCInfo) {
	if len(s.emitted) < ptsRecentEmitted {
		s.emitted = append(s.emitted, poc)
		return
	}
	s.emitted[s.emittedPos] = poc
	s.emittedPos = (s.emittedPos + 1) % ptsRecentEmitted
}
