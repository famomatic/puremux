package media

import (
	"context"
	"errors"
	"io"
	"math"
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
