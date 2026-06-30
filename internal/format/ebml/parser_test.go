package ebml

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestReadHeader(t *testing.T) {
	// Cluster (0x1F43B675) + size VINT 0x85 (value 5) + 5 payload bytes.
	in := []byte{0x1F, 0x43, 0xB6, 0x75, 0x85, 0xDE, 0xAD, 0xBE, 0xEF, 0x00}
	h, err := ReadHeader(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != 0x1F43B675 {
		t.Errorf("ID = 0x%X want 0x1F43B675", h.ID)
	}
	if h.Size != 5 {
		t.Errorf("Size = %d want 5", h.Size)
	}
	if h.SizeWidth != 1 {
		t.Errorf("SizeWidth = %d want 1", h.SizeWidth)
	}
	if h.Unknown {
		t.Error("should not be unknown size")
	}
}

func TestReadHeaderUnknownSize(t *testing.T) {
	// Segment with unknown size (width 8 sentinel).
	in := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	h, err := ReadHeader(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != 0x18538067 {
		t.Errorf("ID = 0x%X want Segment", h.ID)
	}
	if !h.Unknown {
		t.Error("should be unknown size")
	}
}

func TestPatchSize(t *testing.T) {
	// Reserve a 4-byte size VINT (0x10 00 00 00 = value 0), then patch to 0x1234567.
	// Element: Cluster ID + reserved 4-byte size + payload.
	dst := make([]byte, 0, 8)
	dst = append(dst, 0x1F, 0x43, 0xB6, 0x75)        // Cluster ID
	reserved, _ := EncodeVINTWidth(0, 4)              // 0x10 0x00 0x00 0x00
	dst = append(dst, reserved...)
	dst = append(dst, 0xAA, 0xBB, 0xCC, 0xDD)         // payload placeholder

	// Patch the size at offset 4 (after the 4-byte ID), width 4, to 0x1234567.
	if err := PatchSize(dst, 4, 4, 0x1234567); err != nil {
		t.Fatal(err)
	}
	// Decode the patched size.
	dec, _, err := DecodeVINT(dst[4:8])
	if err != nil {
		t.Fatal(err)
	}
	if dec != 0x1234567 {
		t.Errorf("patched size = 0x%X want 0x1234567", dec)
	}
}

func TestPatchSizeOverflow(t *testing.T) {
	dst := make([]byte, 8)
	// width 1 holds max 126; 200 overflows.
	if err := PatchSize(dst, 0, 1, 200); err != ErrVINTOverflow {
		t.Errorf("expected overflow, got %v", err)
	}
}

func TestPatchSizeBounds(t *testing.T) {
	dst := make([]byte, 4)
	if err := PatchSize(dst, 2, 4, 0); err != ErrShortInput {
		t.Errorf("out-of-bounds should be ErrShortInput, got %v", err)
	}
	if err := PatchSize(dst, -1, 1, 0); err != ErrShortInput {
		t.Errorf("negative offset should be ErrShortInput, got %v", err)
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	cases := [][]byte{
		{},                         // no ID
		{0x1F, 0x43, 0xB6},        // partial ID
		{0x1F, 0x43, 0xB6, 0x75}, // ID but no size
		{0x1F, 0x43, 0xB6, 0x75, 0x85}, // size but no payload (ReadHeader stops before payload)
	}
	for i, c := range cases {
		_, err := ReadHeader(bytes.NewReader(c))
		// the last case has a complete header; only payload is missing which
		// ReadHeader does NOT read. So only cases 0..2 should error.
		if i < 3 && err == nil {
			t.Errorf("case %d: expected error for %s", i, hex.EncodeToString(c))
		}
	}
}