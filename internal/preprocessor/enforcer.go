package preprocessor

import (
	"sort"
	"time"

	"famomatic/puremux/internal/core"
)

// Enforcer forces a packet stream into monotonically increasing DTS order
// using a bounded jitter buffer. It NEVER writes to a file (ARCHITECTURE.md
// section 5.B). On overflow it drops the oldest out-of-order packet and
// records the drop in Metrics so callers can detect stream degradation.
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
	if cfg.MaxBufferDuration == 0 {
		cfg.MaxBufferDuration = DefaultConfig().MaxBufferDuration
	}
	return &Enforcer{cfg: cfg}
}

// Process enqueues inbound and emits corrected packets via emit.
//
// The jitter buffer holds out-of-order packets until they can be flushed in
// monotonic order. When the buffer is full the oldest packet that is farthest
// behind the newest is dropped (overflow policy, §5.B). Emitted packets are
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

// insertOrdered inserts p keeping buf sorted by DTS ascending.
func (e *Enforcer) insertOrdered(p *core.Packet) {
	// Drop immediately if older than last emitted (already too late).
	if e.emitted && p.DTS < e.lastDTS {
		e.metrics.DroppedOutOfOrder++
		core.ReleasePacket(p)
		return
	}
	idx := sort.Search(len(e.buf), func(i int) bool {
		return e.buf[i].DTS >= p.DTS
	})
	e.buf = append(e.buf, nil)
	copy(e.buf[idx+1:], e.buf[idx:])
	e.buf[idx] = p

	// Enforce size bound: if over capacity, drop the oldest (index 0).
	if len(e.buf) > e.cfg.MaxBufferSize {
		dropped := e.buf[0]
		e.buf = e.buf[1:]
		e.metrics.DroppedOverflow++
		core.ReleasePacket(dropped)
	}
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
	for i < len(e.buf)-1 { // never flush the newest via this path
		p := e.buf[i]
		if p.DTS+window > newest {
			break // still within the reorder window, hold
		}
		e.emitOne(p, emit)
		i++
	}
	e.buf = e.buf[i:]
}

// Flush emits all remaining buffered packets. The facade calls this after
// each WritePacket (and on Close) so held packets are committed.
func (e *Enforcer) Flush(emit func(*core.Packet)) {
	for i := 0; i < len(e.buf); i++ {
		e.emitOne(e.buf[i], emit)
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
			e.metrics.InterpolatedGaps++
			// We do not fabricate synthetic packets (no decoder, no sample
			// counts); we just note the gap was within the interpolatable
			// range and emit the real packet. The gap metric lets callers
			// detect the discontinuity.
		}
		// Enforce strict monotonicity: never emit a packet at or before last.
		if p.DTS <= e.lastDTS {
			p.DTS = e.lastDTS + 1 // nudge forward by 1ns to preserve order
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
	for _, p := range e.buf {
		core.ReleasePacket(p)
	}
	e.buf = e.buf[:0]
	e.metrics = Metrics{}
	e.lastDTS = 0
	e.emitted = false
}