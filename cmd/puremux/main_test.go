package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/famomatic/puremux/pkg/media"
)

func TestRunUsageAndInvalidCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "puremux remux") {
		t.Fatalf("help = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"nope"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("invalid command = code %d, stderr %q", code, stderr.String())
	}
}

func TestRunProbeAndRemux(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.webm")
	output := filepath.Join(dir, "output.mkv")
	file, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	muxer, err := media.NewMuxer(file, media.MuxOptions{Format: media.FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	track, err := muxer.AddStream(media.Stream{Type: media.MediaVideo, Codec: media.CodecVP9,
		TimeBase: media.Rational{Num: 1, Den: 1_000}, Width: 16, Height: 16})
	if err != nil {
		t.Fatal(err)
	}
	// VP9 profile-0 key frame: frame_marker=2, profile=0, show_existing=0,
	// frame_type=0, show_frame=1, error_resilient=0 (LSB-first byte 0x82).
	if err := muxer.WritePacket(context.Background(), &media.Packet{StreamIndex: track,
		PTS: media.KnownTimestamp(0), DTS: media.KnownTimestamp(0), Duration: media.KnownTimestamp(40),
		Flags: media.PacketKeyframe, Data: []byte{0x82}}); err != nil {
		t.Fatal(err)
	}
	if err := muxer.Close(); err != nil {
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
	if code := run(context.Background(), []string{"remux", "-o", output, input}, &stdout, &stderr); code != 0 {
		t.Fatalf("remux = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	source, err := media.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	demuxer, err := media.Open(context.Background(), source, media.OpenOptions{})
	if err != nil || demuxer.Info().Format != media.FormatMatroska {
		t.Fatalf("output probe = %+v, %v", demuxer, err)
	}
	_ = demuxer.Close()
}

func TestRunRemuxHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"remux", "-o", "out.webm", "in.webm"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("canceled merge = code %d, stderr %q", code, stderr.String())
	}
}
