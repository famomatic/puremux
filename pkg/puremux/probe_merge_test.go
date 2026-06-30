package puremux

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

// TestProbeThenMergeH264 verifies the "I cannot tell what codecs this file
// carries" workflow end to end: Probe surfaces H.264, the caller picks MKV
// (H.264 is MKV-only), and Merge remuxes the H.264 MP4 into MKV.
func TestProbeThenMergeH264(t *testing.T) {
	dir := t.TempDir()
	// MP4 carrying H.264 (avc1). Payload is an AVCC-framed IDR NAL so the
	// keyframe detector has real bytes to inspect.
	mdatPayload := []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x80, 0x40}
	trak := buildMP4Track("avc1", 1000, 1, 33, 8, 0, 1)
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("avc1", 1000, 1, 33, 8, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)
	in := filepath.Join(dir, "h264.mp4")
	writeBytes(t, in, data)

	// Step 1: Probe to discover the codec (no gate guessing).
	info, err := Probe(in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Container != ContainerMP4 {
		t.Fatalf("container = %s want mp4", info.Container)
	}
	if len(info.Tracks) != 1 || info.Tracks[0].Codec != core.CodecH264 {
		t.Fatalf("probe = %+v want one H.264 track", info.Tracks)
	}

	// Step 2: pick the output container from the actual codecs.
	out, err := ProbeOutputContainer(in)
	if err != nil {
		t.Fatalf("ProbeOutputContainer: %v", err)
	}
	if out != ContainerMKV {
		t.Errorf("output = %s want mkv (H.264 is MKV-only)", out)
	}

	// Step 3: Merge into MKV.
	mkvPath := filepath.Join(dir, "out.mkv")
	if err := Merge(context.Background(), []string{in}, mkvPath, DefaultConfig()); err != nil {
		t.Fatalf("Merge H.264 MP4->MKV: %v", err)
	}
	b, _ := readAll(mkvPath)
	if !bytes.Contains(b, []byte("matroska")) {
		t.Error("MKV output missing matroska doctype")
	}
	if !bytes.Contains(b, []byte("V_MPEG4/ISO/AVC")) {
		t.Error("MKV output missing V_MPEG4/ISO/AVC codec id")
	}
	// The AVCC IDR payload (0x65 0x88 0x80 0x40) should appear in the output.
	if !bytes.Contains(b, []byte{0x65, 0x88, 0x80, 0x40}) {
		t.Error("MKV output missing H.264 sample payload")
	}
}

func readAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}
