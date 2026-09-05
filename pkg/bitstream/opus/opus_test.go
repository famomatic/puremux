package opus

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDOPSFromRFC7845OpusHead(t *testing.T) {
	// RFC 7845 OpusHead fields are little-endian. This stereo header has
	// pre-skip 312 (0x0138), input rate 48000, gain -2, and mapping family 0.
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0xfe, 0xff, 0)
	dops, err := DOPSFromHead(head)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0xff, 0xfe, 0}
	if !bytes.Equal(dops, want) {
		t.Fatalf("dOps = %x, want %x", dops, want)
	}
	back, err := HeadFromDOPS(dops)
	if err != nil || !bytes.Equal(back, head) {
		t.Fatalf("inverse header = %x, %v", back, err)
	}
	c, err := ParseDOPS(dops)
	if err != nil || c.PreSkip != 312 || c.InputSampleRate != 48000 || c.OutputGain != -2 {
		t.Fatalf("config = %+v, error = %v", c, err)
	}
	if binary.LittleEndian.Uint16(head[10:12]) != binary.BigEndian.Uint16(dops[2:4]) {
		t.Fatal("pre-skip endian conversion failed")
	}
}

func TestOpusConfigBoundaries(t *testing.T) {
	bad := [][]byte{nil, []byte("OpusHead"), append([]byte("BadHead!"), make([]byte, 11)...)}
	for i, data := range bad {
		if _, err := DOPSFromHead(data); err == nil {
			t.Fatalf("case %d unexpectedly passed", i)
		}
	}
	if _, err := ParseDOPS([]byte{0, 2}); err == nil {
		t.Fatal("truncated dOps passed")
	}
	for _, data := range [][]byte{nil, {0, 2}, {1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		if _, err := HeadFromDOPS(data); err == nil {
			t.Fatalf("invalid inverse config accepted: %x", data)
		}
	}
	// RFC 7845 mapping indices address the N+M decoded channels, not the
	// output channel count C. With N=1 and M=0, index 1 is out of range.
	badHead := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 1, 1, 0, 0, 1)
	if _, err := ParseHead(badHead); err == nil {
		t.Fatal("out-of-range OpusHead channel mapping accepted")
	}
	badDOPS := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 1, 1, 0, 0, 1}
	if _, err := ParseDOPS(badDOPS); err == nil {
		t.Fatal("out-of-range dOps channel mapping accepted")
	}

	// C need not equal N+M. This is structurally valid: three decoded
	// channels feed two output channels, and both indices are below N+M=3.
	validDOPS := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 1, 2, 1, 0, 2}
	if _, err := ParseDOPS(validDOPS); err != nil {
		t.Fatalf("valid N+M mapping rejected: %v", err)
	}
}
