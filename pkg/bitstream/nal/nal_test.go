package nal

import (
	"bytes"
	"testing"
)

func TestLengthPrefixAnnexBRoundTrip(t *testing.T) {
	// ISO BMFF NAL length fields are unsigned big-endian; Annex B uses
	// 00 00 00 01/00 00 01 byte-stream start codes.
	prefixed := []byte{0, 0, 0, 2, 0x65, 0x88, 0, 0, 0, 1, 0x41}
	annex, err := LengthPrefixedToAnnexB(prefixed, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 1, 0x65, 0x88, 0, 0, 0, 1, 0x41}
	if !bytes.Equal(annex, want) {
		t.Fatalf("Annex B = %x", annex)
	}
	back, err := AnnexBToLengthPrefixed(annex, 4)
	if err != nil || !bytes.Equal(back, prefixed) {
		t.Fatalf("round trip = %x, %v", back, err)
	}
}

func TestNALBoundaries(t *testing.T) {
	for _, test := range []struct {
		data []byte
		size int
	}{{nil, 4}, {[]byte{0, 0, 0}, 4}, {[]byte{0, 0, 0, 0}, 4}, {[]byte{2, 1}, 1}, {[]byte{1, 1}, 3}} {
		if _, err := LengthPrefixedToAnnexB(test.data, test.size); err == nil && len(test.data) != 0 {
			t.Fatalf("malformed length packet accepted: %x/%d", test.data, test.size)
		}
	}
	for _, data := range [][]byte{nil, {0, 0, 1}, {1, 2, 3}} {
		if _, err := AnnexBToLengthPrefixed(data, 4); err == nil {
			t.Fatalf("malformed Annex B accepted: %x", data)
		}
	}
}
