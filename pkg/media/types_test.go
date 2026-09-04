package media

import (
	"context"
	"errors"
	"io"
	"math"
	"math/big"
	"math/rand"
	"testing"
	"time"
)

func TestRationalDurationExactAndOverflow(t *testing.T) {
	got, ok := (Rational{Num: 1, Den: 48000}).Duration(960)
	if !ok || got != 20*time.Millisecond {
		t.Fatalf("Duration = %v, %v; want 20ms, true", got, ok)
	}
	if _, ok := (Rational{}).Duration(1); ok {
		t.Fatal("invalid rational reported successful conversion")
	}
	if _, ok := (Rational{Num: math.MaxInt64, Den: 1}).Duration(math.MaxInt64); ok {
		t.Fatal("overflowing conversion reported success")
	}
}

func TestRationalRescale(t *testing.T) {
	from := Rational{Num: 1, Den: 48_000}
	to := Rational{Num: 1, Den: 1_000_000_000}
	if got, ok := from.Rescale(960, to); !ok || got != 20_000_000 {
		t.Fatalf("Rescale = %d, %v", got, ok)
	}
	if _, ok := from.Rescale(math.MaxInt64, Rational{Num: 1, Den: math.MaxInt64}); ok {
		t.Fatal("overflowing Rescale unexpectedly succeeded")
	}
}

func TestRationalRescaleMatchesBigIntOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(0x505552454d5558))
	values := []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64}
	for range 2_000 {
		values = append(values, int64(rng.Uint64()))
	}
	for i, value := range values {
		from := Rational{Num: nonZeroInt64(rng), Den: positiveInt64(rng)}
		to := Rational{Num: nonZeroInt64(rng), Den: positiveInt64(rng)}
		got, ok := from.Rescale(value, to)
		want, wantOK := bigIntRescale(value, from.Num, to.Den, from.Den, to.Num)
		if got != want || ok != wantOK {
			t.Fatalf("case %d: %d * %d * %d / (%d * %d) = %d, %v; want %d, %v", i, value, from.Num, to.Den, from.Den, to.Num, got, ok, want, wantOK)
		}
	}
}

func TestRationalConversionsDoNotAllocate(t *testing.T) {
	if got := testing.AllocsPerRun(1_000, func() {
		_, _ = (Rational{Num: 1, Den: 48_000}).Rescale(960, Rational{Num: 1, Den: 1_000_000_000})
	}); got != 0 {
		t.Fatalf("Rescale allocations = %v, want 0", got)
	}
	if got := testing.AllocsPerRun(1_000, func() {
		_, _ = (Rational{Num: 1, Den: 48_000}).Duration(960)
	}); got != 0 {
		t.Fatalf("Duration allocations = %v, want 0", got)
	}
}

func nonZeroInt64(rng *rand.Rand) int64 {
	for {
		if value := int64(rng.Uint64()); value != 0 {
			return value
		}
	}
}

func positiveInt64(rng *rand.Rand) int64 {
	return int64(rng.Uint64()>>1) + 1
}

func bigIntRescale(value, n1, n2, d1, d2 int64) (int64, bool) {
	n := big.NewInt(value)
	n.Mul(n, big.NewInt(n1))
	n.Mul(n, big.NewInt(n2))
	n.Quo(n, big.NewInt(d1))
	n.Quo(n, big.NewInt(d2))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

func TestTimestampDistinguishesZeroFromUnknown(t *testing.T) {
	if (Timestamp{}).Valid {
		t.Fatal("zero Timestamp must be unknown")
	}
	ts := KnownTimestamp(0)
	if !ts.Valid || ts.Value != 0 {
		t.Fatalf("KnownTimestamp(0) = %+v", ts)
	}
}

func TestPacketReleaseIsIdempotent(t *testing.T) {
	called := 0
	p := &Packet{Data: []byte{1, 2, 3}, release: func(b []byte) {
		called++
		if len(b) != 3 {
			t.Errorf("released len = %d, want 3", len(b))
		}
	}}
	p.Release()
	p.Release()
	if called != 1 || p.Data != nil {
		t.Fatalf("called=%d data=%v", called, p.Data)
	}
}

func TestMemorySourceCapabilitiesAndClose(t *testing.T) {
	s := MemorySource("fixture", []byte("abcdef"))
	if s.Name() != "fixture" || s.Size() != 6 {
		t.Fatalf("name=%q size=%d", s.Name(), s.Size())
	}
	b := make([]byte, 2)
	if _, err := s.ReadAt(b, 3); err != nil || string(b) != "de" {
		t.Fatalf("ReadAt = %q, %v", b, err)
	}
	seekable, ok := s.(io.Seeker)
	if !ok {
		t.Fatal("memory source is not seekable")
	}
	if _, err := seekable.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(b); err != nil || string(b) != "bc" {
		t.Fatalf("Read = %q, %v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cs := s.(ContextRandomAccessSource)
	if _, err := cs.ReadAtContext(ctx, b, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAtContext error = %v, want context.Canceled", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(b); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after Close error = %v, want ErrClosed", err)
	}
}
