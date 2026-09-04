package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/famomatic/puremux/pkg/puremux"
)

func TestRunUsageAndInvalidCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "puremux merge") {
		t.Fatalf("help = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"nope"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("invalid command = code %d, stderr %q", code, stderr.String())
	}
}

func TestRunProbeAndMerge(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.webm")
	output := filepath.Join(dir, "output.mkv")
	file, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	session, err := puremux.NewSession(file, puremux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	track, err := session.AddTrack(puremux.Track{Codec: puremux.CodecVP9, IsVideo: true, Width: 16, Height: 16})
	if err != nil {
		t.Fatal(err)
	}
	// VP9 profile-0 key frame: frame_marker=2, profile=0, show_existing=0,
	// frame_type=0, show_frame=1, error_resilient=0 (LSB-first byte 0x82).
	if err := session.WritePacket(&puremux.Packet{TrackID: track, PTS: 0, DTS: 0, Data: []byte{0x82}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"probe", input}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "container: webm") || !strings.Contains(stdout.String(), "video vp9") {
		t.Fatalf("probe = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"merge", "-o", output, input}, &stdout, &stderr); code != 0 {
		t.Fatalf("merge = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if info, err := puremux.Probe(output); err != nil || info.Container != puremux.ContainerMKV {
		t.Fatalf("output probe = %+v, %v", info, err)
	}
}

func TestRunMergeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"merge", "-o", "out.webm", "in.webm"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("canceled merge = code %d, stderr %q", code, stderr.String())
	}
}
