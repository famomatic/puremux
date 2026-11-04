package media

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestSeparateInitBudgetAndDiagnostic(t *testing.T) {
	data := auditSegment(t, 0)
	cut := bytes.Index(data, []byte("moof")) - 4
	var stats OpenStats
	_, err := OpenMP4WithInitOptions(context.Background(), data[:cut], MemorySource("seg", data[cut:]), OpenOptions{MaxInitBytes: 1, OnOpen: func(s OpenStats) { stats = s }})
	if !errors.Is(err, ErrInitLimit) || !errors.Is(stats.Err, ErrInitLimit) || stats.BytesRead != 1 {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
	d, err := OpenMP4WithInitOptions(context.Background(), data[:cut], MemorySource("seg", data[cut:]), OpenOptions{MaxInitBytes: int64(len(data)) * 3})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
}
