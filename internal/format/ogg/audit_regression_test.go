package ogg

import (
	"bytes"
	"context"
	"testing"
)

func TestAuditOpusEndTrimmingDoesNotShiftPacketStart(t *testing.T) {
	// RFC7845 4.4: EOS granule1440 after previous960 keeps480 samples of
	// final960-sample packet. Start remains960; final packet needs480 trim.
	// RFC6716 TOC F8 = config31 (11111), stereo0, frame-count code00; 960 samples.
	// OpusHead LE fields: pre-skip0, 48000=80 BB 00 00. Ogg granule LE
	// 960=C0 03 00 00 00 00 00 00; 1440=A0 05 00 00 00 00 00 00.
	head := append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	data := append(makePage(2, 0, 1, 0, head), makePage(0, 0, 1, 1, opusTags("audit"))...)
	data = append(data, makePage(0, 960, 1, 2, []byte{0xf8})...)
	data = append(data, makePage(4, 1440, 1, 3, []byte{0xf8})...)
	r, e := NewReader(bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if _, e = r.NextPacket(context.Background()); e != nil {
		t.Fatal(e)
	}
	p, e := r.NextPacket(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if p.PTS != 960 || p.DiscardSamples != 480 {
		t.Fatalf("EOS packet start shifted to %d; expected960 (trim tail480, not shift start)", p.PTS)
	}
}

func TestOggContinuedPacketBound(t *testing.T) {
	// Ogg lacing is unsigned bytes; 255 continues a packet, <255 ends it.
	// A continued page has bit 0 set. No codec payload inspection is involved.
	partial := make([]byte, (16<<20)-1)
	p := page{flags: 1, lacing: []byte{1}, body: []byte{0}}
	complete, _, _, err := splitPagePackets(p, partial, 0)
	if err != nil || len(complete) != 1 || len(complete[0].data) != 16<<20 {
		t.Fatalf("exact bound: %v", err)
	}
	p.lacing = []byte{2}
	p.body = []byte{0, 0}
	if _, _, _, err := splitPagePackets(p, partial, 0); err == nil {
		t.Fatal("oversize continued packet accepted")
	}
	p.body = nil
	if _, _, _, err := splitPagePackets(p, partial, 0); err == nil {
		t.Fatal("truncated page accepted")
	}
}
