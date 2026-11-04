package webm

import (
	"bytes"
	"context"
	"testing"
)

func TestAudit20260906WebMReadRetry(t *testing.T) {
	data, available := lazyWebMFixture() // Existing RFC8794/9559 header fixture; opaque payload.
	spool := &gatedSpool{Reader: bytes.NewReader(data), available: int64(available)}
	rd, err := NewDemuxReaderWithSize(spool, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	spool.available = int64(available - 1) // Block header available; block payload read fails.
	if _, err = rd.NextPacket(context.Background()); err == nil {
		t.Fatal("fault not reached")
	}
	spool.available = int64(len(data))
	p, err := rd.NextPacket(context.Background())
	if err != nil {
		t.Fatalf("retry cannot recover block boundary: %v", err)
	}
	if !bytes.Equal(p.Data, []byte{0x11}) {
		t.Fatalf("failed read lost first packet: %x", p.Data)
	}
}
