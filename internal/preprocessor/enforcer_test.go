package preprocessor

import (
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
