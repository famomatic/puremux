package preprocessor

import (
	"math"
	"sort"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// Enforcer forces a packet stream into monotonically increasing DTS order
// using a bounded jitter buffer. It NEVER writes to a file (ARCHITECTURE.md
// section 5.B). Capacity pressure emits the oldest packet to ensure progress;
// packets arriving behind the emitted watermark are counted and dropped.
type Enforcer struct {
	cfg     Config
	metrics Metrics
	buf     []*core.Packet // ordered by DTS ascending
	lastDTS time.Duration  // last emitted DTS (for monotonicity + gap detection)
	emitted bool
}

// NewEnforcer builds an Enforcer with the given config, clamped to safe bounds.
func NewEnforcer(cfg Config) *Enforcer {
	if cfg.MaxBufferSize < 1 {
		cfg.MaxBufferSize = 1
	}
	if cfg.MaxBufferDuration > math.MaxInt64 {
		cfg.MaxBufferDuration = math.MaxInt64
	}
	if cfg.MinMonotonicStep > math.MaxInt64 {
		cfg.MinMonotonicStep = math.MaxInt64
	}
	return &Enforcer{cfg: cfg}
}

// Process enqueues inbound and emits corrected packets via emit.
//
// The jitter buffer holds out-of-order packets until they can be flushed in
// monotonic order. Capacity pressure releases the oldest packet. Emitted packets are
// pool-acquired; the caller owns and must release them.
func (e *Enforcer) Process(inbound *core.Packet, emit func(*core.Packet)) error {
	if inbound == nil {
		return nil
	}
	// Insert maintaining DTS order.
	e.insertOrdered(inbound)

	// Flush packets that are safe to emit: those whose DTS is <= the newest
	// buffered DTS minus the reorder window, OR when the buffer is full.
	e.flushReady(emit)
	return nil
}

// insertOrdered inserts p keeping buf sorted by DTS ascending. The insert is
// STABLE: among equal DTS values the new packet goes after the existing ones,
// preserving arrival order. With `>=` (first equal index) a duplicate-stamped
// run came back out reversed — a live backfill burst [IDR, F1..F9] all
// stamped t0 emitted as [F9..F1, IDR], so the keyframe Aligner dropped every
// frame ahead of the reordered IDR and the whole burst was lost.
func (e *Enforcer) insertOrdered(p *core.Packet) {
	// Drop immediately if older than last emitted (already too late).
	if e.emitted && p.DTS < e.lastDTS {
		e.metrics.DroppedOutOfOrder++
		core.ReleasePacket(p)
		return
	}
	idx := sort.Search(len(e.buf), func(i int) bool {
		return e.buf[i].DTS > p.DTS
	})
	e.buf = append(e.buf, nil)
	copy(e.buf[idx+1:], e.buf[idx:])
	e.buf[idx] = p

}

// flushReady emits packets that can no longer be overtaken: any packet
// whose DTS is at least MaxBufferDuration behind the newest buffered DTS.
// Packets within the reorder window are held so a slightly out-of-order
// successor can still slot ahead. The newest is always held until the caller
// invokes Flush (so the facade's per-packet Flush commits it).
func (e *Enforcer) flushReady(emit func(*core.Packet)) {
	if len(e.buf) == 0 {
		return
	}
	newest := e.buf[len(e.buf)-1].DTS
	window := time.Duration(e.cfg.MaxBufferDuration)
	i := 0
	for i < len(e.buf) {
		full := len(e.buf)-i > e.cfg.MaxBufferSize
		if !full && i == len(e.buf)-1 {
			break
		}
		p := e.buf[i]
		if !full && window == 0 {
			break
		}
		if !full && (p.DTS > time.Duration(math.MaxInt64)-window || p.DTS+window > newest) {
			break // still within the reorder window, hold
		}
		e.emitOne(p, emit)
		i++
	}
	// Compact the buffer so emitted pointers do not linger in the backing
	// array (the old e.buf = e.buf[i:] form kept them referenced).
	copy(e.buf, e.buf[i:])
	for j := len(e.buf) - i; j < len(e.buf); j++ {
		e.buf[j] = nil
	}
	e.buf = e.buf[:len(e.buf)-i]
}

// Flush emits all remaining buffered packets. The facade calls this after
// each WritePacket (and on Close) so held packets are committed.
func (e *Enforcer) Flush(emit func(*core.Packet)) {
	for i := 0; i < len(e.buf); i++ {
		e.emitOne(e.buf[i], emit)
		e.buf[i] = nil // don't retain emitted (now caller-owned) pointers
	}
	e.buf = e.buf[:0]
}

// emitOne interpolates small gaps then emits p, enforcing strict monotonicity.
func (e *Enforcer) emitOne(p *core.Packet, emit func(*core.Packet)) {
	if e.emitted {
		gap := p.DTS - e.lastDTS
		// Synthesize across small gaps only; leave large gaps as discontinuities.
		gapThreshold := time.Duration(e.cfg.InterpolationGapThreshold)
		if gapThreshold > 0 && gap > 0 && gap <= gapThreshold && gap >= time.Millisecond {
			e.metrics.DetectedGaps++
			// We do not fabricate synthetic packets (no decoder, no sample
			// counts); we just note the gap was within the interpolatable
			// range and emit the real packet. The gap metric lets callers
			// detect the discontinuity.
		}
		// Enforce strict monotonicity: never emit a packet at or before last.
		// The nudge is MinMonotonicStep (default 1ns) so that containers with
		// coarse clocks (90 kHz MPEG-TS, 1ms Matroska) still see distinct
		// timestamps after quantization. PTS is shifted by the same amount to
		// preserve the packet's PTS-DTS offset (B-frame reorder delay).
		if p.DTS <= e.lastDTS {
			if e.lastDTS == time.Duration(math.MaxInt64) {
				// There is no representable timestamp greater than MaxInt64.
				// Dropping is safer than wrapping negative or emitting a duplicate.
				e.metrics.DroppedOutOfOrder++
				core.ReleasePacket(p)
				return
			}
			step := time.Duration(e.cfg.MinMonotonicStep)
			if step <= 0 {
				step = 1
			}
			target := e.lastDTS
			if target <= time.Duration(math.MaxInt64)-step {
				target += step
			} else {
				target = time.Duration(math.MaxInt64)
			}
			shift := target - p.DTS
			p.DTS = target
			if shift > 0 && p.PTS > time.Duration(math.MaxInt64)-shift {
				p.PTS = time.Duration(math.MaxInt64)
			} else {
				p.PTS += shift
			}
		}
	}
	e.lastDTS = p.DTS
	e.emitted = true
	emit(p)
}

// Metrics returns a snapshot of observable side effects (§5.B overflow policy).
func (e *Enforcer) Metrics() Metrics { return e.metrics }

// Reset clears all internal state for reuse on a fresh stream.
func (e *Enforcer) Reset() {
	for i, p := range e.buf {
		core.ReleasePacket(p)
		e.buf[i] = nil
	}
	e.buf = e.buf[:0]
	e.metrics = Metrics{}
	e.lastDTS = 0
	e.emitted = false
}
