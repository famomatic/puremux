package ebml

import (
	"bytes"
	"testing"
)

func TestVINTWidth(t *testing.T) {
	cases := []struct {
		b    byte
		want int
	}{
		{0x80, 1},  // 1xxxxxxx
		{0x40, 2},  // 01xxxxxx
		{0x20, 3},  // 001xxxxx
		{0x10, 4},  // 0001xxxx
		{0x08, 5},
		{0x04, 6},
		{0x02, 7},
		{0x01, 8},  // 00000001 (marker occupies whole first byte)
		{0x00, 0},  // invalid: no marker
		{0xFF, 1},  // 1xxxxxxx (width 1, all data set)
	}
	for _, tc := range cases {
		if got := VINTWidth(tc.b); got != tc.want {
			t.Errorf("VINTWidth(0x%02X) = %d want %d", tc.b, got, tc.want)
		}
	}
}

func TestEncodeVINTWidth1(t *testing.T) {
	// width 1: marker bit7, data bits 6..0.
	// val 0 => 0x80. val 126 => 0xFE. val 127 => overflow for width 1 standard,
	// but is the unknown sentinel (0xFF).
	cases := []struct {
		val  uint64
		want []byte
	}{
		{0, []byte{0x80}},
		{1, []byte{0x81}},
		{126, []byte{0xFE}},
	}
	for _, tc := range cases {
		got, err := EncodeVINT(tc.val)
		if err != nil {
			t.Errorf("EncodeVINT(%d) err: %v", tc.val, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("EncodeVINT(%d) = % X want % X", tc.val, got, tc.want)
		}
	}
}

func TestEncodeVINTWidth2(t *testing.T) {
	// width 2: byte0 = 01 dddddd, byte1 = dddddddd (14 data bits).
	// val 0 => 0x40 0x00. val 1 => 0x40 0x01.
	// val 127 => byte0=0x40 (top 6 bits 000000), byte1=0x7F => 0x40 0x7F.
	// val 16382 (max) => 0x7F 0xFE.
	cases := []struct {
		val  uint64
		want []byte
	}{
		{0, []byte{0x40, 0x00}},
		{1, []byte{0x40, 0x01}},
		{127, []byte{0x40, 0x7F}},
		{16382, []byte{0x7F, 0xFE}},
	}
	for _, tc := range cases {
		got, err := EncodeVINTWidth(tc.val, 2)
		if err != nil {
			t.Errorf("EncodeVINTWidth(%d,2) err: %v", tc.val, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("EncodeVINTWidth(%d,2) = % X want % X", tc.val, got, tc.want)
		}
	}
}

func TestEncodeVINTWidth4(t *testing.T) {
	// width 4: byte0 = 0001 dddd, marker at bit4, 28 data bits.
	// val 0 => 0x10 0x00 0x00 0x00.
	// val 1 => 0x10 0x00 0x00 0x01.
	got, err := EncodeVINTWidth(1, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x10, 0x00, 0x00, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("width4 val1 = % X want % X", got, want)
	}
}

func TestVINTRoundTrip(t *testing.T) {
	vals := []uint64{0, 1, 126, 127, 255, 16382, 16383, 65535, 268435454}
	for _, v := range vals {
		enc, err := EncodeVINT(v)
		if err != nil {
			t.Errorf("EncodeVINT(%d) err: %v", v, err)
			continue
		}
		dec, w, err := DecodeVINT(enc)
		if err != nil {
			t.Errorf("DecodeVINT(% X) err: %v", enc, err)
			continue
		}
		if dec != v {
			t.Errorf("roundtrip %d => % X => %d", v, enc, dec)
		}
		if w != len(enc) {
			t.Errorf("width mismatch: got %d want %d", w, len(enc))
		}
	}
}

func TestEncodeVINTOverflow(t *testing.T) {
	// value exceeds max width-8 data (56 bits).
	_, err := EncodeVINT(uint64(1) << 57)
	if err != ErrVINTOverflow {
		t.Errorf("expected ErrVINTOverflow, got %v", err)
	}
	// width too small
	_, err = EncodeVINTWidth(256, 1) // 256 > 126
	if err != ErrVINTOverflow {
		t.Errorf("width1 256 should overflow, got %v", err)
	}
}

func TestEncodeVINTUnknownSize(t *testing.T) {
	cases := []struct {
		width int
		want  []byte
	}{
		{1, []byte{0xFF}},                     // 1 + 1111111
		{2, []byte{0x7F, 0xFF}},                // 01 + 111111 11111111
		{4, []byte{0x1F, 0xFF, 0xFF, 0xFF}},    // 0001 + 1111 ...
		{8, []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, tc := range cases {
		got, err := EncodeVINTUnknown(tc.width)
		if err != nil {
			t.Errorf("EncodeVINTUnknown(%d) err: %v", tc.width, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("EncodeVINTUnknown(%d) = % X want % X", tc.width, got, tc.want)
		}
		if !IsUnknownSize(got, tc.width) {
			t.Errorf("IsUnknownSize should be true for width %d unknown", tc.width)
		}
	}
}

func TestDecodeVINTInvalid(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x00},            // no marker
		{0x40},           // width 2 but truncated
		{0x10, 0x00, 0x00}, // width 4 truncated
	}
	for i, c := range cases {
		_, _, err := DecodeVINT(c)
		if err == nil {
			t.Errorf("case %d: expected error for % X", i, c)
		}
	}
}

func TestEncodeVINTWidthInvalid(t *testing.T) {
	if _, err := EncodeVINTWidth(0, 0); err != ErrVINTInvalid {
		t.Errorf("width 0 should be invalid, got %v", err)
	}
	if _, err := EncodeVINTWidth(0, 9); err != ErrVINTInvalid {
		t.Errorf("width 9 should be invalid, got %v", err)
	}
	if _, err := EncodeVINTUnknown(0); err != ErrVINTInvalid {
		t.Errorf("unknown width 0 should be invalid, got %v", err)
	}
}
