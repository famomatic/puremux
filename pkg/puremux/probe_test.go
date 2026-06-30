package puremux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

func TestProbeWebM(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	writeWebMFile(t, in) // VP9 + Opus

	info, err := Probe(in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Container != ContainerWebM {
		t.Errorf("container = %s want webm", info.Container)
	}
	if len(info.Tracks) != 2 {
		t.Fatalf("got %d tracks want 2", len(info.Tracks))
	}
	// Track 1 = VP9 video, track 2 = Opus audio.
	want := map[int]core.CodecType{1: core.CodecVP9, 2: core.CodecOpus}
	got := map[int]core.CodecType{}
	for _, tr := range info.Tracks {
		got[tr.Number] = tr.Codec
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("track %d codec = %s want %s", n, got[n], c)
		}
	}
	// Verify track kinds.
	for _, tr := range info.Tracks {
		if tr.Number == 1 && tr.Kind != core.TrackVideo {
			t.Error("track 1 should be video")
		}
		if tr.Number == 2 && tr.Kind != core.TrackAudio {
			t.Error("track 2 should be audio")
		}
	}
}

func TestProbeMKV(t *testing.T) {
	dir := t.TempDir()
	// Build a WebM input, remux to MKV, then probe the MKV.
	in := filepath.Join(dir, "in.webm")
	writeWebMFile(t, in)
	mkvOut := filepath.Join(dir, "out.mkv")
	if err := Merge(context.Background(), []string{in}, mkvOut, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	info, err := Probe(mkvOut)
	if err != nil {
		t.Fatalf("Probe MKV: %v", err)
	}
	if info.Container != ContainerMKV {
		t.Errorf("container = %s want mkv", info.Container)
	}
	// MKV carries the same VP9+Opus codecs.
	codecs := map[core.CodecType]bool{}
	for _, tr := range info.Tracks {
		codecs[tr.Codec] = true
	}
	if !codecs[core.CodecVP9] || !codecs[core.CodecOpus] {
		t.Errorf("MKV probe missing VP9/Opus, got %v", codecs)
	}
}

func TestProbeMP4(t *testing.T) {
	dir := t.TempDir()
	mdatPayload := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB}
	trak := buildMP4Track("vp09", 1000, 2, 33, 4, 0, 1)
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("vp09", 1000, 2, 33, 4, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)
	in := filepath.Join(dir, "in.mp4")
	writeBytes(t, in, data)

	info, err := Probe(in)
	if err != nil {
		t.Fatalf("Probe MP4: %v", err)
	}
	if info.Container != ContainerMP4 {
		t.Errorf("container = %s want mp4", info.Container)
	}
	if len(info.Tracks) != 1 {
		t.Fatalf("got %d tracks want 1", len(info.Tracks))
	}
	if info.Tracks[0].Codec != core.CodecVP9 {
		t.Errorf("codec = %s want vp9", info.Tracks[0].Codec)
	}
	if info.Tracks[0].Kind != core.TrackVideo {
		t.Error("track should be video")
	}
}

func TestProbeH264MP4(t *testing.T) {
	dir := t.TempDir()
	// MP4 carrying H.264 (avc1).
	mdatPayload := []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x80, 0x40}
	trak := buildMP4Track("avc1", 1000, 1, 33, 8, 0, 1)
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("avc1", 1000, 1, 33, 8, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)
	in := filepath.Join(dir, "h264.mp4")
	writeBytes(t, in, data)

	info, err := Probe(in)
	if err != nil {
		t.Fatalf("Probe H.264 MP4: %v", err)
	}
	if info.Tracks[0].Codec != core.CodecH264 {
		t.Errorf("codec = %s want h264", info.Tracks[0].Codec)
	}
}

func TestProbeOutputContainerWebM(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	writeWebMFile(t, in) // VP9+Opus -> WebM fits

	out, err := ProbeOutputContainer(in)
	if err != nil {
		t.Fatalf("ProbeOutputContainer: %v", err)
	}
	if out != ContainerWebM {
		t.Errorf("got %s want webm", out)
	}
}

func TestProbeOutputContainerMKVForFLAC(t *testing.T) {
	// Build an MKV with FLAC audio: WebM cannot hold FLAC, so the probe must
	// pick MKV. We synthesize by writing a WebM then probing is not enough; we
	// instead test the codec-set logic directly through CanRemuxCodecs.
	if CanRemuxCodecs(ContainerWebM, []core.CodecType{core.CodecFLAC}) {
		t.Error("FLAC should not fit WebM")
	}
	if !CanRemuxCodecs(ContainerMKV, []core.CodecType{core.CodecFLAC}) {
		t.Error("FLAC should fit MKV")
	}
}

func TestProbeOutputContainerH264PicksMKV(t *testing.T) {
	// H.264 is MKV-only (not in WebM). A file whose only codec is H.264 must
	// route to MKV, not WebM.
	if CanRemuxCodecs(ContainerWebM, []core.CodecType{core.CodecH264}) {
		t.Error("H.264 should not fit WebM")
	}
	if !CanRemuxCodecs(ContainerMKV, []core.CodecType{core.CodecH264}) {
		t.Error("H.264 should fit MKV")
	}
}

func TestProbeBadInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "garbage.bin")
	writeBytes(t, in, []byte("not a media file at all"))
	_, err := Probe(in)
	if err == nil {
		t.Error("Probe on garbage should error")
	}
}

// writeBytes writes b to path.
func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := writeFile(path, b); err != nil {
		t.Fatal(err)
	}
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
