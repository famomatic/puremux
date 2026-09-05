package media

import (
	"context"
	"errors"
	"testing"
)

func TestOpenInitializationBudgetAndDiagnostics(t *testing.T) {
	for _, budget := range []int64{1, 4, 12} {
		calls := 0
		var stats OpenStats
		_, err := Open(context.Background(), MemorySource("budget", []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 1, 0x42, 0xf7, 0x81, 1}), OpenOptions{MaxInitBytes: budget, OnOpen: func(s OpenStats) { stats = s; calls++ }})
		if !errors.Is(err, ErrInitLimit) {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if calls != 1 || stats.BytesRead > budget || stats.ReadCalls == 0 || stats.Err != err || stats.Elapsed < 0 {
			t.Fatalf("stats: %+v, calls %d", stats, calls)
		}
	}
}
func TestInitializationBudgetDisablesAfterOpen(t *testing.T) {
	// RFC7845 little-endian OpusHead and granules exercise header-only Open;
	// later packet reads must not remain constrained by its byte budget.
	var stats OpenStats
	d, err := Open(context.Background(), MemorySource("ogg", append(mediaTestOggPage(2, 0, 1, 0, append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)), append(mediaTestOggPage(0, 0, 1, 1, mediaTestOpusTags("test")), mediaTestOggPage(4, 960, 1, 2, []byte{0xf8, 0xff, 0xfe})...)...)), OpenOptions{MaxInitBytes: 128, OnOpen: func(s OpenStats) { stats = s }})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if stats.Format != FormatOgg || stats.Phase != "initialize" || stats.Err != nil {
		t.Fatalf("stats: %+v", stats)
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
}
