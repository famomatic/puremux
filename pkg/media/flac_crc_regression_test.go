package media

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestFLACPublishedFrameCRC(t *testing.T) {
	// RFC9639 D.1.1 complete published example. MSB-first header at offset42,
	// 15-byte frame, footer AA9A. Compressed subframes are never decoded here.
	data, _ := hex.DecodeString("664c6143800000221000100000000f00000f0ac442f0000000013e84b41807dc690307586a3dad1a2e0ffff869180000bf0358fd03128baa9a")
	d, err := Open(context.Background(), MemorySource("example.flac", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if len(p.Data) != 15 || p.Duration.Value != 1 {
		t.Fatalf("packet=%+v", p)
	}
	if _, err = d.ReadPacket(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{data[:len(data)-1], append(append([]byte(nil), data[:len(data)-1]...), 0)} {
		if d, err := Open(context.Background(), MemorySource("bad.flac", bad), OpenOptions{}); err == nil {
			d.Close()
			t.Fatal("bad frame checksum accepted")
		}
	}
}
