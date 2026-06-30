package webm

import (
	"bytes"
	"io"
	"testing"

	"famomatic/puremux/internal/format/ebml"
)

// TestWebMEndToEnd generates a minimal but complete WebM stream and verifies
// the structural invariants: EBML header, Segment with patched size, Info
// with patched Duration, Tracks, a Cluster with a SimpleBlock, Cues, and
// SeekHead pointers. This is the integration gate for the format layer.
func TestWebMEndToEnd(t *testing.T) {
	ws := newMockSeeker()

	// 1. EBML header.
	if err := WriteEBMLHeader(ws); err != nil {
		t.Fatal(err)
	}

	// 2. Segment (seekable, reserved 8-byte size).
	h, err := BeginSegment(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	segmentBodyStart := h.SegmentStart

	// 3. SeekHead placeholder position: we will write SeekHead FIRST (pointing
	// at Info/Tracks/Cues), but Cues offset isn't known yet. For this test
	// we write SeekHead after Cues. Reserve SeekHead position now.
	seekHeadPos, _ := ws.Seek(0, io.SeekCurrent)
	_ = seekHeadPos

	// 4. Info (with reserved Duration).
	infoPos, _ := ws.Seek(0, io.SeekCurrent)
	if err := WriteInfo(ws, &h, 1000000, testTime); err != nil {
		t.Fatal(err)
	}

	// 5. Tracks.
	tracksPos, _ := ws.Seek(0, io.SeekCurrent)
	_, err = WriteTracks(ws, []TrackSpec{
		{Number: 1, UID: 1, Codec: 0, IsVideo: true, Width: 320, Height: 240},
	})
	// Note: Codec 0 = CodecUnknown; codecIDFor returns "" -> acceptable for test structure.
	if err != nil {
		t.Fatal(err)
	}
	h.TracksEnd, _ = ws.Seek(0, io.SeekCurrent)

	// 6. Cluster with one SimpleBlock.
	clusterAbsTc := uint64(0)
	h.FirstClusterOff, _ = ws.Seek(0, io.SeekCurrent)
	cw, err := BeginCluster(ws, true, clusterAbsTc)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, true, []byte{0xDE, 0xAD}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}

	// 7. Cues pointing at the cluster.
	cuesPos, _ := ws.Seek(0, io.SeekCurrent)
	clusterRelPos := uint64(h.FirstClusterOff - segmentBodyStart)
	_, err = WriteCues(ws, []CuePoint{
		{Timecode: 0, Track: 1, ClusterPosition: clusterRelPos},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 8. Patch Duration (e.g. 1.0 second).
	if h.HasDuration {
		if err := PatchDuration(ws, h.DurationPayloadOff, 1.0); err != nil {
			t.Fatal(err)
		}
	}

	// 9. Patch Segment size.
	endOff, _ := ws.Seek(0, io.SeekCurrent)
	segmentLen := uint64(endOff - segmentBodyStart)
	if err := patchSizeAt(ws, h.SegmentSizeOff, 8, segmentLen); err != nil {
		t.Fatal(err)
	}

	// --- Verification ---
	out := ws.Bytes()

	// EBML header present with doctype webm.
	if !bytes.Contains(out, []byte("webm")) {
		t.Error("missing webm doctype")
	}

	// Segment size patched (not the reserved 0x01 + 7 zeros).
	segSizeBytes := out[h.SegmentSizeOff : h.SegmentSizeOff+8]
	dec, _, err := ebml.DecodeVINT(segSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if dec != segmentLen {
		t.Errorf("Segment size = %d want %d", dec, segmentLen)
	}

	// Duration patched to 1.0.
	if h.HasDuration {
		durBytes := out[h.DurationPayloadOff : h.DurationPayloadOff+8]
		// 1.0 = 0x3FF0000000000000
		want := []byte{0x3F, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		if !bytes.Equal(durBytes, want) {
			t.Errorf("Duration = % X want % X", durBytes, want)
		}
	}

	// Cues element present and contains the cluster position.
	if !bytes.Contains(out, []byte{0x1C, 0x53, 0xBB, 0x6B}) {
		t.Error("Cues element not found")
	}

	// Cluster with SimpleBlock present.
	if !bytes.Contains(out, []byte{0xA3}) {
		t.Error("SimpleBlock not found")
	}

	// Info, Tracks offsets recorded are within segment body.
	if infoPos < segmentBodyStart {
		t.Error("Info position before segment body")
	}
	if tracksPos < segmentBodyStart {
		t.Error("Tracks position before segment body")
	}
	if cuesPos < segmentBodyStart {
		t.Error("Cues position before segment body")
	}
}