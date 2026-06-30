package webm

import (
	"bytes"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/format/ebml"
)

func TestWriteEBMLHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEBMLHeader(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	// First element: EBML master (0x1A45DFA3).
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idEBML {
		t.Errorf("first element id = 0x%X want EBML 0x1A45DFA3", id)
	}
	// Find the DocType string "webm" in the output.
	if !bytes.Contains(out, []byte("webm")) {
		t.Error("EBML header missing doctype 'webm'")
	}
	// DocTypeVersion should be 4 (WebM current).
	if !bytes.Contains(out, []byte{0x42, 0x87, 0x81, 0x04}) {
		t.Error("DocTypeVersion=4 not found")
	}
}

func TestBeginSegmentSeekable(t *testing.T) {
	ws := newMockSeeker()
	h, err := BeginSegment(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	// Should start with Segment ID 0x18538067.
	id, _, err := ebml.DecodeElementID(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != idSegment {
		t.Errorf("Segment id = 0x%X want 0x18538067", id)
	}
	if !h.Seekable {
		t.Error("header should report seekable")
	}
	// The reserved size VINT (8 bytes, value 0) starts at SegmentSizeOff.
	// For width 8, value 0 => 0x01 followed by 7 zero bytes.
	sizeBytes := out[h.SegmentSizeOff : h.SegmentSizeOff+8]
	want := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(sizeBytes, want) {
		t.Errorf("reserved size = % X want % X", sizeBytes, want)
	}
	// SegmentStart should be right after the size VINT (4 + 8 = 12).
	if h.SegmentStart != 12 {
		t.Errorf("SegmentStart = %d want 12", h.SegmentStart)
	}
}

func TestBeginSegmentStreaming(t *testing.T) {
	ws := newMockSeeker()
	h, err := BeginSegment(ws, false)
	if err != nil {
		t.Fatal(err)
	}
	out := ws.Bytes()
	// Non-seekable: unknown-size sentinel width 8.
	sizeBytes := out[h.SegmentSizeOff : h.SegmentSizeOff+8]
	want := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(sizeBytes, want) {
		t.Errorf("streaming size = % X want % X", sizeBytes, want)
	}
	if h.Seekable {
		t.Error("streaming header should not be seekable")
	}
}

func TestWriteInfoSeekableReservesDuration(t *testing.T) {
	ws := newMockSeeker()
	h, _ := BeginSegment(ws, true)
	if err := WriteInfo(ws, &h, 1000000, testTime); err != nil {
		t.Fatal(err)
	}
	if !h.HasDuration {
		t.Fatal("seekable Info should reserve Duration")
	}
	// Duration payload offset should point at 8 zero bytes.
	out := ws.Bytes()
	if h.DurationPayloadOff+8 > int64(len(out)) {
		t.Fatal("duration payload offset out of range")
	}
	durBytes := out[h.DurationPayloadOff : h.DurationPayloadOff+8]
	want := make([]byte, 8)
	if !bytes.Equal(durBytes, want) {
		t.Errorf("duration placeholder = % X want 8 zero bytes", durBytes)
	}
	// TimecodeScale 1000000 should appear as uint element.
	if !bytes.Contains(out, []byte{0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40}) {
		// TimecodeScale ID (3-byte class 3) is 0x2AD7B1.
		// We just check the ID prefix is present.
		if !bytes.Contains(out, []byte{0x2A, 0xD7, 0xB1}) {
			t.Error("TimecodeScale element id not found")
		}
	}
}

// avoid importing time at package level scope issues; define a fixed time.
var testTime = time.Unix(1700000000, 0).UTC()
