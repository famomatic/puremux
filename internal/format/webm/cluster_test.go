package webm

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/famomatic/puremux/internal/format/ebml"
)

func TestClusterSeekablePatchesSize(t *testing.T) {
	ws := newMockSeeker()
	cw, err := BeginCluster(ws, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, true, []byte{0xAA}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()

	// Verify Cluster ID.
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idCluster {
		t.Errorf("Cluster id = 0x%X want 0x1F43B675", id)
	}
	// Verify Timestamp element present (id 0xE7, value 1000).
	if !bytes.Contains(out, []byte{0xE7, 0x82, 0x03, 0xE8}) {
		t.Error("Timestamp 1000 (0xE7 0x82 0x03 0xE8) not found")
	}
	// The size VINT (width 4) at sizeOff should now be patched to the real
	// cluster length, not 0.
	sizeBytes := out[cw.sizeOff : cw.sizeOff+4]
	// It should NOT be all zeros (the reserved placeholder).
	if bytes.Equal(sizeBytes, []byte{0x10, 0x00, 0x00, 0x00}) {
		t.Error("cluster size was not patched (still reserved zeros)")
	}
	// Decode the patched size and verify it matches actual content length.
	dec, _, err := ebml.DecodeVINT(sizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	expectedLen := uint64(len(out) - 4 /*id*/ - 4 /*size width*/)
	if dec != expectedLen {
		t.Errorf("patched size = %d want %d", dec, expectedLen)
	}
}

func TestBlockGroupExactBytesAndBoundaries(t *testing.T) {
	ws := newMockSeeker()
	cw, err := BeginCluster(ws, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteBlockGroup(1, -2, false, 40, -128, []byte{0x65}); err != nil {
		t.Fatal(err)
	}
	// Matroska Block is track VINT 0x81, signed big-endian timecode -2,
	// flags 0, payload 0x65. Duration 40 is uint 0x28; ReferenceBlock 0
	// marks an unknown dependency, and DiscardPadding -128 is a minimal
	// signed one-byte integer.
	want := []byte{
		0xA0, 0x91,
		0xA1, 0x85, 0x81, 0xff, 0xfe, 0x00, 0x65,
		0x9B, 0x81, 0x28,
		0xFB, 0x81, 0x00,
		0x75, 0xA2, 0x81, 0x80,
	}
	if !bytes.Contains(ws.Bytes(), want) {
		t.Fatalf("BlockGroup bytes not found:\n got % X\nwant % X", ws.Bytes(), want)
	}
	for _, tc := range []struct {
		track, duration uint64
	}{
		{0, 1}, {16_384, 1}, {1, 0},
	} {
		if err := cw.WriteBlockGroup(tc.track, 0, true, tc.duration, 0, nil); err == nil {
			t.Fatalf("track=%d duration=%d unexpectedly accepted", tc.track, tc.duration)
		}
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteBlockGroup(1, 0, true, 1, 0, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close = %v", err)
	}
}

func TestClusterStreamingUnknownSize(t *testing.T) {
	ws := newMockSeeker()
	cw, err := BeginCluster(ws, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, true, []byte{0xBB}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	// Streaming: unknown-size sentinel width 4 = 0x1F 0xFF 0xFF 0xFF.
	sizeBytes := out[cw.sizeOff : cw.sizeOff+4]
	want := []byte{0x1F, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(sizeBytes, want) {
		t.Errorf("streaming cluster size = % X want % X", sizeBytes, want)
	}
}

func TestClusterTimecodeAndBlock(t *testing.T) {
	ws := newMockSeeker()
	cw, err := BeginCluster(ws, true, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if cw.Timecode() != 5000 {
		t.Errorf("Timecode = %d want 5000", cw.Timecode())
	}
	if err := cw.WriteSimpleBlock(1, 100, true, []byte{0x11, 0x22}); err != nil {
		t.Fatal(err)
	}
	_ = cw.Close()
	out := ws.Bytes()
	// SimpleBlock element: 0xA3 + size + block payload.
	// Block payload = track1(0x81) + tc100(0x00 0x64) + flags(0x80) + 0x11 0x22.
	blkPayload := []byte{0x81, 0x00, 0x64, 0x80, 0x11, 0x22}
	if !bytes.Contains(out, append([]byte{0xA3, 0x86}, blkPayload...)) {
		t.Errorf("SimpleBlock element not found in cluster")
	}
}

func TestClusterDoubleClose(t *testing.T) {
	ws := newMockSeeker()
	cw, _ := BeginCluster(ws, true, 0)
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	// Second close should be a no-op (no panic, no double-patch).
	if err := cw.Close(); err != nil {
		t.Errorf("double close error: %v", err)
	}
}
