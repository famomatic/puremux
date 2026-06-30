package webm

import (
	"bytes"
	"testing"
)

// TestReaderRoundTrip builds a minimal WebM via the puremux writer primitives,
// then reads it back with the Reader to verify the demux matches what was
// written. This is a self-contained test (no external fixtures).
func TestReaderRoundTrip(t *testing.T) {
	ws := newMockSeeker()
	// Write EBML header + Segment + Info + one track + one cluster with a block.
	if err := WriteEBMLHeader(ws); err != nil {
		t.Fatal(err)
	}
	h, err := BeginSegment(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteInfo(ws, &h, 1000000, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTracks(ws, []TrackSpec{
		{Number: 1, UID: 1, Codec: 0, IsVideo: true, Width: 16, Height: 16},
	}); err != nil {
		t.Fatal(err)
	}
	cw, err := BeginCluster(ws, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, true, []byte{0xDE, 0xAD}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}

	// Read back.
	rd, err := NewReader(bytes.NewReader(ws.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	tracks := rd.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
	if tracks[0].Number != 1 || !tracks[0].IsVideo {
		t.Errorf("track = %+v", tracks[0])
	}
	blk, err := rd.NextBlock()
	if err != nil {
		t.Fatalf("NextBlock: %v", err)
	}
	if blk.TrackNum != 1 || !blk.Keyframe {
		t.Errorf("block = %+v", blk)
	}
	if !bytes.Equal(blk.Data, []byte{0xDE, 0xAD}) {
		t.Errorf("block data = % X want DE AD", blk.Data)
	}
	if blk.AbsTimecode() != 0 {
		t.Errorf("abs timecode = %d want 0", blk.AbsTimecode())
	}
	// Next should be EOF.
	if _, err := rd.NextBlock(); err == nil {
		t.Error("expected EOF after last block")
	}
}

func TestReaderInvalidInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x00}, // no EBML marker
		{0xFF}, // bad VINT
	}
	for i, c := range cases {
		if _, err := NewReader(bytes.NewReader(c)); err == nil {
			t.Errorf("case %d: expected error for invalid input", i)
		}
	}
}