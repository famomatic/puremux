package webm

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"famomatic/puremux/internal/format/ebml"

	"famomatic/puremux/internal/core"
)

func TestPatchDuration(t *testing.T) {
	ws := newMockSeeker()
	// Reserve 8 zero bytes at offset 4 (after a fake 4-byte ID).
	ws.Write([]byte{0x18, 0x53, 0x80, 0x67}) // Segment ID placeholder
	ws.Write(make([]byte, 8))                // duration placeholder at offset 4
	if err := PatchDuration(ws, 4, 10.5); err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	got := math.Float64frombits(binary.BigEndian.Uint64(out[4:12]))
	if got != 10.5 {
		t.Errorf("patched duration = %v want 10.5", got)
	}
	// Verify exact bytes against IEEE-754 spec.
	want := []byte{0x40, 0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(out[4:12], want) {
		t.Errorf("duration bytes = % X want % X", out[4:12], want)
	}
}

func TestWriteCues(t *testing.T) {
	ws := newMockSeeker()
	cues := []CuePoint{
		{Timecode: 0, Track: 1, ClusterPosition: 100},
		{Timecode: 5000, Track: 1, ClusterPosition: 2000},
	}
	start, err := WriteCues(ws, cues)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 {
		t.Errorf("Cues start = %d want 0", start)
	}
	out := ws.Bytes()
	// Verify Cues ID.
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idCues {
		t.Errorf("id = 0x%X want Cues", id)
	}
	// Should contain two CuePoint entries (id 0xBB).
	if bytes.Count(out, []byte{0xBB}) < 2 {
		t.Error("expected at least 2 CuePoint elements")
	}
}

func TestWriteSeekHead(t *testing.T) {
	ws := newMockSeeker()
	if err := WriteSeekHead(ws, 10, 200, 5000); err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idSeekHead {
		t.Errorf("id = 0x%X want SeekHead", id)
	}
	// Should contain 3 Seek entries (id 0x4DBB).
	if bytes.Count(out, []byte{0x4D, 0xBB}) < 3 {
		t.Error("expected 3 Seek entries (Info, Tracks, Cues)")
	}
	// Verify the SeekID for Info (0x1549A966) appears as a binary element.
	infoIDBytes, _ := ebml.EncodeElementID(idInfo)
	if !bytes.Contains(out, infoIDBytes) {
		t.Error("SeekHead missing Info SeekID")
	}
}

func TestWriteSeekHeadNoCues(t *testing.T) {
	// Streaming mode: cuesPos = -1 means no Cues entry.
	ws := newMockSeeker()
	if err := WriteSeekHead(ws, 10, 200, -1); err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	// Only 2 Seek entries (Info, Tracks), no Cues.
	if bytes.Count(out, []byte{0x4D, 0xBB}) != 2 {
		t.Errorf("expected 2 Seek entries (no Cues), got %d", bytes.Count(out, []byte{0x4D, 0xBB}))
	}
}

func TestWriteTracks(t *testing.T) {
	ws := newMockSeeker()
	tracks := []TrackSpec{
		{Number: 1, UID: 123, Codec: core.CodecVP9, IsVideo: true, Width: 1920, Height: 1080},
		{Number: 2, UID: 456, Codec: core.CodecOpus, IsVideo: false, Channels: 2, SampleRate: 48000.0},
	}
	start, err := WriteTracks(ws, tracks)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 {
		t.Errorf("Tracks start = %d want 0", start)
	}
	out := ws.Bytes()
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idTracks {
		t.Errorf("id = 0x%X want Tracks", id)
	}
	// Should contain 2 TrackEntry (id 0xAE).
	if bytes.Count(out, []byte{0xAE}) < 2 {
		t.Error("expected 2 TrackEntry elements")
	}
	// VP9 codec ID string should appear.
	if !bytes.Contains(out, []byte("V_VP9")) {
		t.Error("VP9 codec ID not found")
	}
	// Opus codec ID string should appear.
	if !bytes.Contains(out, []byte("A_OPUS")) {
		t.Error("Opus codec ID not found")
	}
}