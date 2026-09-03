package webm

import (
	"bytes"
	"testing"
)

func TestParseBlockPayloadLacingModes(t *testing.T) {
	header := func(flags byte, body ...byte) []byte {
		return append([]byte{0x81, 0x00, 0x05, flags}, body...)
	}
	tests := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{"none", header(0x80, 1, 2), [][]byte{{1, 2}}},
		// Matroska lacing: count byte stores frame_count-1. Xiph sizes encode
		// every size except the final one as 255 runs plus a remainder.
		{"xiph", header(0x82, 2, 2, 3, 1, 2, 3, 4, 5, 6), [][]byte{{1, 2}, {3, 4, 5}, {6}}},
		{"fixed", header(0x84, 2, 1, 2, 3, 4, 5, 6), [][]byte{{1, 2}, {3, 4}, {5, 6}}},
		// EBML lace first size 2 is 0x82. The next size delta +1 is stored
		// as 64 (one-byte signed bias 63), hence VINT byte 0xC0.
		{"ebml", header(0x86, 2, 0x82, 0xC0, 1, 2, 3, 4, 5, 6), [][]byte{{1, 2}, {3, 4, 5}, {6}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBlockPayload(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.track != 1 || got.relTimecode != 5 {
				t.Fatalf("header = track %d tc %d", got.track, got.relTimecode)
			}
			if len(got.frames) != len(tt.want) {
				t.Fatalf("frame count = %d, want %d", len(got.frames), len(tt.want))
			}
			for i := range tt.want {
				if !bytes.Equal(got.frames[i].data, tt.want[i]) {
					t.Fatalf("frame %d = %v, want %v", i, got.frames[i].data, tt.want[i])
				}
			}
		})
	}
}

func TestParseBlockPayloadLacingBoundaries(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x81},
		{0x80, 0, 0, 0},                      // track zero
		{0x81, 0, 0, 0x02},                   // no lace count
		{0x81, 0, 0, 0x02, 1},                // Xiph size missing
		{0x81, 0, 0, 0x04, 1, 1},             // fixed payload not divisible
		{0x81, 0, 0, 0x06, 1},                // EBML first size missing
		{0x81, 0, 0, 0x06, 2, 0x85, 0xC0, 1}, // sizes overrun
	}
	for i, in := range cases {
		if _, err := parseBlockPayload(in); err == nil {
			t.Errorf("case %d (% x): expected error", i, in)
		}
	}
}
