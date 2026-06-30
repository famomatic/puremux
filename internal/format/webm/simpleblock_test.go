package webm

import (
	"bytes"
	"testing"
)

func TestEncodeSimpleBlockTrack1Keyframe(t *testing.T) {
	payload := []byte{0xAA, 0xBB}
	got := EncodeSimpleBlock(1, 100, true, payload)
	// track1 VINT 0x81, timecode 100 = 0x00 0x64, flags 0x80, payload.
	want := []byte{0x81, 0x00, 0x64, 0x80, 0xAA, 0xBB}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestEncodeSimpleBlockInterframe(t *testing.T) {
	got := EncodeSimpleBlock(1, 0, false, []byte{0xCC})
	// flags should NOT have keyframe bit.
	want := []byte{0x81, 0x00, 0x00, 0x00, 0xCC}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestEncodeSimpleBlockNegativeTimecode(t *testing.T) {
	got := EncodeSimpleBlock(1, -100, true, nil)
	// timecode -100 = 0xFF 0x9C (int16 big-endian two's complement).
	want := []byte{0x81, 0xFF, 0x9C, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestEncodeSimpleBlockWideTrack(t *testing.T) {
	got := EncodeSimpleBlock(200, 0, true, nil)
	// track 200 needs width-2 VINT: 0x40 0xC8.
	want := []byte{0x40, 0xC8, 0x00, 0x00, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestEncodeSimpleBlockMaxTimecode(t *testing.T) {
	// Cluster-relative timecode is int16: max 32767.
	got := EncodeSimpleBlock(1, 32767, true, nil)
	want := []byte{0x81, 0x7F, 0xFF, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestEncodeSimpleBlockMinTimecode(t *testing.T) {
	// min int16 = -32768 = 0x80 0x00.
	got := EncodeSimpleBlock(1, -32768, true, nil)
	want := []byte{0x81, 0x80, 0x00, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X want % X", got, want)
	}
}
