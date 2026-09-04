package preprocessor

import (
	"math"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

func collectPackets(n int) []*core.Packet {
	out := make([]*core.Packet, 0, n)
	collect := func(p *core.Packet) { out = append(out, p) }
	_ = collect
	return out
}

func TestEnforcerMonotonicOrder(t *testing.T) {
	e := NewEnforcer(Config{MaxBufferSize: 10, MaxBufferDuration: 100_000_000})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// Feed packets out of order: 30ms, 10ms, 20ms.
	dts := []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	for _, d := range dts {
		p := core.AcquirePacket()
		p.DTS = d
		p.PTS = d
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	// Force flush by feeding a far-future packet.
	p := core.AcquirePacket()
	p.DTS = 1 * time.Second
	e.Process(p, emit)
	e.Flush(emit)

	// Emitted order must be monotonic ascending by DTS.
	if len(got) < 3 {
		t.Fatalf("got %d packets want >=3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].DTS <= got[i-1].DTS {
			t.Errorf("non-monotonic at %d: %v <= %v", i, got[i].DTS, got[i-1].DTS)
		}
	}
	// Release.
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerDropsLatePackets(t *testing.T) {
	e := NewEnforcer(Config{MaxBufferSize: 10, MaxBufferDuration: 50_000_000})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	// Emit a packet at 100ms, which sets lastDTS.
	p := core.AcquirePacket()
	p.DTS = 100 * time.Millisecond
	e.Process(p, emit)

	// Flush.
	pf := core.AcquirePacket()
	pf.DTS = 200 * time.Millisecond
	e.Process(pf, emit)

	// Now feed a late packet at 50ms (before last emitted 100ms).
	late := core.AcquirePacket()
	late.DTS = 50 * time.Millisecond
	e.Process(late, emit)

	m := e.Metrics()
	if m.DroppedOutOfOrder == 0 {
		t.Error("expected DroppedOutOfOrder > 0 for late packet")
	}

	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerOverflowDrops(t *testing.T) {
	// With a large reorder window, packets are held (not flushed) so the
	// buffer fills. Feeding 5 packets with increasing DTS, each within the
	// window of the newest, all stay buffered. cap=3 so inserts 4 and 5
	// overflow and drop the oldest.
	e := NewEnforcer(Config{MaxBufferSize: 3, MaxBufferDuration: uint64(10 * time.Second)})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	for i := 0; i < 5; i++ {
		p := core.AcquirePacket()
		p.DTS = time.Duration(i) * time.Second
		e.Process(p, emit) // no Flush: packets accumulate
	}

	m := e.Metrics()
	if m.DroppedOverflow == 0 {
		t.Errorf("expected DroppedOverflow > 0, got %d (emitted=%d)", m.DroppedOverflow, len(got))
	}

	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerReset(t *testing.T) {
	e := NewEnforcer(Config{MaxBufferSize: 10, MaxBufferDuration: 100_000_000})
	emit := func(p *core.Packet) {}
	p := core.AcquirePacket()
	p.DTS = 5 * time.Millisecond
	e.Process(p, emit)
	e.Reset()
	m := e.Metrics()
	if m.DroppedOverflow != 0 || m.DroppedOutOfOrder != 0 {
		t.Error("Reset should clear metrics")
	}
}

func TestEnforcerMinMonotonicStepDuplicates(t *testing.T) {
	// Simulates a live backfill burst: several packets stamped with the SAME
	// timestamp. With MinMonotonicStep = 1ms, emitted DTS must advance by at
	// least 1ms per packet so 90 kHz / 1ms-clock containers still see
	// strictly increasing timestamps after quantization.
	e := NewEnforcer(Config{
		MaxBufferSize:     10,
		MaxBufferDuration: 1, // effectively no reorder hold
		MinMonotonicStep:  uint64(time.Millisecond),
	})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }

	const dup = 5
	base := 100 * time.Millisecond
	ptsLead := 7 * time.Millisecond // PTS-DTS offset that must be preserved
	for i := 0; i < dup; i++ {
		p := core.AcquirePacket()
		p.DTS = base
		p.PTS = base + ptsLead
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	e.Flush(emit)

	if len(got) != dup {
		t.Fatalf("got %d packets, want %d", len(got), dup)
	}
	for i, p := range got {
		wantDTS := base + time.Duration(i)*time.Millisecond
		if p.DTS != wantDTS {
			t.Errorf("packet %d DTS = %v, want %v", i, p.DTS, wantDTS)
		}
		if p.PTS-p.DTS != ptsLead {
			t.Errorf("packet %d PTS-DTS offset = %v, want %v", i, p.PTS-p.DTS, ptsLead)
		}
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerDefaultNudgeIs1ns(t *testing.T) {
	// Zero MinMonotonicStep keeps the original 1ns behavior.
	e := NewEnforcer(Config{MaxBufferSize: 10, MaxBufferDuration: 1})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	for i := 0; i < 2; i++ {
		p := core.AcquirePacket()
		p.DTS = 50 * time.Millisecond
		p.PTS = 50 * time.Millisecond
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	e.Flush(emit)
	if len(got) != 2 {
		t.Fatalf("got %d packets", len(got))
	}
	if d := got[1].DTS - got[0].DTS; d != 1 {
		t.Errorf("nudge = %v, want 1ns", d)
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerStableOrderForEqualDTS(t *testing.T) {
	// Packets sharing one DTS must emit in ARRIVAL order. The old first-equal
	// insertion point reversed duplicate runs, which put a burst's keyframe
	// last and let the Aligner drop the whole burst.
	e := NewEnforcer(Config{MaxBufferSize: 16, MaxBufferDuration: 1})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	for i := 0; i < 4; i++ {
		p := core.AcquirePacket()
		p.DTS = 100 * time.Millisecond
		p.Data = append(p.Data[:0], byte(i))
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	e.Flush(emit)
	if len(got) != 4 {
		t.Fatalf("got %d packets, want 4", len(got))
	}
	for i, p := range got {
		if len(p.Data) != 1 || p.Data[0] != byte(i) {
			t.Fatalf("packet %d out of arrival order: payload %v", i, p.Data)
		}
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerZeroDurationDisablesTimeFlush(t *testing.T) {
	e := NewEnforcer(Config{MaxBufferSize: 4, MaxBufferDuration: 0})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	for _, dts := range []time.Duration{0, time.Second} {
		p := core.AcquirePacket()
		p.DTS = dts
		p.PTS = dts
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("duration-disabled buffer emitted %d packets before Flush", len(got))
	}
	e.Flush(emit)
	if len(got) != 2 {
		t.Fatalf("Flush emitted %d packets, want 2", len(got))
	}
	for _, p := range got {
		core.ReleasePacket(p)
	}
}

func TestEnforcerTimestampSaturationDoesNotWrap(t *testing.T) {
	e := NewEnforcer(Config{
		MaxBufferSize:     4,
		MaxBufferDuration: 1,
		MinMonotonicStep:  math.MaxUint64,
	})
	var got []*core.Packet
	emit := func(p *core.Packet) { got = append(got, p) }
	for i := 0; i < 3; i++ {
		p := core.AcquirePacket()
		p.DTS = time.Duration(math.MaxInt64 - 1)
		p.PTS = p.DTS
		if err := e.Process(p, emit); err != nil {
			t.Fatal(err)
		}
	}
	e.Flush(emit)
	defer func() {
		for _, p := range got {
			core.ReleasePacket(p)
		}
	}()
	if len(got) != 2 {
		t.Fatalf("emitted %d packets, want 2 (unrepresentable third timestamp dropped)", len(got))
	}
	if got[0].DTS != time.Duration(math.MaxInt64-1) || got[1].DTS != time.Duration(math.MaxInt64) {
		t.Fatalf("DTS values = [%v, %v], want MaxInt64-1 then MaxInt64", got[0].DTS, got[1].DTS)
	}
	if got[1].PTS != time.Duration(math.MaxInt64) {
		t.Fatalf("saturated PTS = %v, want MaxInt64", got[1].PTS)
	}
	if e.Metrics().DroppedOutOfOrder != 1 {
		t.Fatalf("DroppedOutOfOrder = %d, want 1", e.Metrics().DroppedOutOfOrder)
	}
}
