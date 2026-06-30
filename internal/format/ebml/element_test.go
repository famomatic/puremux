package ebml

import (
	"bytes"
	"testing"
)

func TestEncodeElementIDClasses(t *testing.T) {
	// Class widths from RFC 8794. The id's leading bit position sets width.
	cases := []struct {
		id   uint32
		want []byte
	}{
		// Class 1 (1 byte): 0x81..0xFF
		{0x81, []byte{0x81}},
		{0xFF, []byte{0xFF}},
		{0xA3, []byte{0xA3}},
		// Class 2 (2 bytes): 0x407..0x7FF
		// Class 2 (2 bytes): 0x4000..0x7FFF. id includes marker; stored as id BE.
		{0x4001, []byte{0x40, 0x01}},
		{0x6264, []byte{0x62, 0x64}}, // Block class 2 id
		// Class 4 (4 bytes): WebM/Matroska top-level IDs
		{0x18538067, []byte{0x18, 0x53, 0x80, 0x67}}, // Segment
		{0x1A45DFA3, []byte{0x1A, 0x45, 0xDF, 0xA3}}, // EBML
		{0x1549A966, []byte{0x15, 0x49, 0xA9, 0x66}}, // Info
		{0x1654AE6B, []byte{0x16, 0x54, 0xAE, 0x6B}}, // Tracks
		{0x1F43B675, []byte{0x1F, 0x43, 0xB6, 0x75}}, // Cluster
	}
	for _, tc := range cases {
		got, err := EncodeElementID(tc.id)
		if err != nil {
			t.Errorf("EncodeElementID(0x%X) err: %v", tc.id, err)
			continue
		}
		if tc.want != nil && !bytes.Equal(got, tc.want) {
			t.Errorf("EncodeElementID(0x%X) = % X want % X", tc.id, got, tc.want)
		}
		// Round trip.
		dec, w, err := DecodeElementID(got)
		if err != nil {
			t.Errorf("DecodeElementID(% X) err: %v", got, err)
			continue
		}
		if dec != tc.id {
			t.Errorf("roundtrip 0x%X => % X => 0x%X", tc.id, got, dec)
		}
		if w != len(got) {
			t.Errorf("width mismatch: %d vs %d", w, len(got))
		}
	}
}

func TestEncodeElementIDInvalid(t *testing.T) {
	cases := []uint32{0, 0x80, 0x100, 0x20000000, 0x40000000}
	for _, id := range cases {
		if _, err := EncodeElementID(id); err == nil {
			t.Errorf("EncodeElementID(0x%X) should be invalid", id)
		}
	}
}

func TestEncodeElement(t *testing.T) {
	// Cluster (0x1F43B675) with a 5-byte payload.
	var buf bytes.Buffer
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}
	if err := EncodeElement(&buf, 0x1F43B675, payload); err != nil {
		t.Fatal(err)
	}
	// Expected: ID (4 bytes) + size VINT (1 byte, value 5 => 0x85) + payload.
	want := []byte{0x1F, 0x43, 0xB6, 0x75, 0x85, 0xDE, 0xAD, 0xBE, 0xEF, 0x00}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("EncodeElement = % X want % X", buf.Bytes(), want)
	}
}

func TestEncodeElementEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeElement(&buf, 0xA3, nil); err != nil {
		t.Fatal(err)
	}
	// ID 0xA3 + size 0x80 (VINT value 0).
	want := []byte{0xA3, 0x80}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("empty element = % X want % X", buf.Bytes(), want)
	}
}

func TestEncodeElementUnknownSize(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeElementUnknownSize(&buf, 0x18538067, 8); err != nil {
		t.Fatal(err)
	}
	// Segment ID (4 bytes) + unknown size width 8 = 0x01 FF FF FF FF FF FF FF.
	want := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("unknown-size = % X want % X", buf.Bytes(), want)
	}
}

func TestDecodeElementIDTruncated(t *testing.T) {
	cases := [][]byte{
		nil, {}, {0x18}, {0x18, 0x53}, {0x18, 0x53, 0x80},
	}
	for i, c := range cases {
		if _, _, err := DecodeElementID(c); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
