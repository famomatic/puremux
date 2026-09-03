package core

import (
	"testing"
	"time"
)

func TestOpusPacketSamplesRFC6716TOC(t *testing.T) {
	// The TOC config occupies bits 7..3 and frame-count code bits 1..0.
	// Config 19 is CELT 20 ms (960 samples); config 3 is SILK 60 ms.
	tests := []struct {
		name string
		in   []byte
		want int
	}{
		{"celt 20ms one frame", []byte{19 << 3}, 960},
		{"celt 20ms two CBR frames", []byte{19<<3 | 1}, 1920},
		{"silk 60ms two VBR frames", []byte{3<<3 | 2}, 5760},
		{"code 3 six 20ms frames", []byte{19<<3 | 3, 6}, 5760},
		{"code 3 truncated", []byte{19<<3 | 3}, 0},
		{"code 3 zero frames", []byte{19<<3 | 3, 0}, 0},
		{"over 120ms forbidden", []byte{19<<3 | 3, 7}, 0},
		{"empty", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OpusPacketSamples(tt.in); got != tt.want {
				t.Fatalf("OpusPacketSamples(% x) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
	if got := OpusPacketDuration([]byte{19 << 3}); got != 20*time.Millisecond {
		t.Fatalf("duration = %v, want 20ms", got)
	}
}
