package puremux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/webm"
)

// writeWebMFile produces a minimal valid WebM file on disk by driving the
// internal webm writer directly with a VP9+Opus track pair. This gives the
// Merge/RemuxInputs tests a spec-accurate input to round-trip.
func writeWebMFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ws := &fileSeeker{f: f}
	if err := webm.WriteEBMLHeader(ws); err != nil {
		t.Fatal(err)
	}
	h, err := webm.BeginSegment(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	segStart := h.SegmentStart
	if _, err := webm.WriteTracks(ws, []webm.TrackSpec{
		{Number: 1, UID: 1, Codec: core.CodecVP9, IsVideo: true, Width: 320, Height: 240},
		{Number: 2, UID: 2, Codec: core.CodecOpus, Channels: 2, SampleRate: 48000},
	}); err != nil {
		t.Fatal(err)
	}
	cw, err := webm.BeginCluster(ws, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, true, []byte{0xDE, 0xAD}); err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(2, 0, false, []byte{0xBE, 0xEF}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	end, _ := ws.Seek(0, 1)
	if err := webm.PatchSegmentSize(ws, h.SegmentSizeOff, 8, uint64(end-int64(segStart))); err != nil {
		t.Fatal(err)
	}
}

// fileSeeker adapts *os.File into the writeSeeker interface used by the webm pkg.
type fileSeeker struct{ f *os.File }

func (fs *fileSeeker) Write(p []byte) (int, error)        { return fs.f.Write(p) }
func (fs *fileSeeker) Seek(o int64, w int) (int64, error) { return fs.f.Seek(o, w) }

func TestMergeWebMToWebM(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	out := filepath.Join(dir, "out.webm")
	writeWebMFile(t, in)

	if err := Merge(context.Background(), []string{in}, out, DefaultConfig()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("webm")) {
		t.Error("output missing webm doctype")
	}
	if !bytes.Contains(b, []byte("V_VP9")) || !bytes.Contains(b, []byte("A_OPUS")) {
		t.Error("output missing VP9/Opus codec ids")
	}
	if len(b) == 0 {
		t.Fatal("empty output")
	}
}

func TestMergeWebMToMKV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	out := filepath.Join(dir, "out.mkv")
	writeWebMFile(t, in)

	if err := Merge(context.Background(), []string{in}, out, DefaultConfig()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// MKV output must declare the matroska doctype, not webm.
	if !bytes.Contains(b, []byte("matroska")) {
		t.Error("MKV output missing matroska doctype")
	}
	if bytes.Contains(b, []byte("webm")) {
		t.Error("MKV output should not contain webm doctype")
	}
	if !bytes.Contains(b, []byte("V_VP9")) || !bytes.Contains(b, []byte("A_OPUS")) {
		t.Error("output missing VP9/Opus codec ids")
	}
}

func TestMergeRejectsUnsupportedOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	out := filepath.Join(dir, "out.mp4") // MP4 output not supported
	writeWebMFile(t, in)

	err := Merge(context.Background(), []string{in}, out, DefaultConfig())
	if !errors.Is(err, ErrUnsupportedOutput) {
		t.Errorf("got err %v want ErrUnsupportedOutput", err)
	}
	// Output file should not be left behind on rejection.
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("output file should not exist after gate rejection")
	}
}

func TestMergeRejectsBadInputMagic(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	// Not an EBML or MP4 file.
	if err := os.WriteFile(in, []byte("not a media file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.webm")
	err := Merge(context.Background(), []string{in}, out, DefaultConfig())
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("got err %v want ErrUnsupportedInput", err)
	}
}

func TestMergeTwoInputsMerged(t *testing.T) {
	// Two single-track WebM inputs (one video, one audio) merged into one WebM.
	dir := t.TempDir()
	vid := filepath.Join(dir, "vid.webm")
	aud := filepath.Join(dir, "aud.webm")
	writeSingleTrackWebM(t, vid, true)
	writeSingleTrackWebM(t, aud, false)
	out := filepath.Join(dir, "out.webm")
	if err := Merge(context.Background(), []string{vid, aud}, out, DefaultConfig()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("V_VP9")) || !bytes.Contains(b, []byte("A_OPUS")) {
		t.Error("merged output missing VP9 or Opus")
	}
}

// writeSingleTrackWebM writes a WebM with one track (video or audio).
func writeSingleTrackWebM(t *testing.T, path string, video bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ws := &fileSeeker{f: f}
	if err := webm.WriteEBMLHeader(ws); err != nil {
		t.Fatal(err)
	}
	h, err := webm.BeginSegment(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	segStart := h.SegmentStart
	var specs []webm.TrackSpec
	if video {
		specs = []webm.TrackSpec{{Number: 1, UID: 1, Codec: core.CodecVP9, IsVideo: true, Width: 320, Height: 240}}
	} else {
		specs = []webm.TrackSpec{{Number: 1, UID: 1, Codec: core.CodecOpus, Channels: 2, SampleRate: 48000}}
	}
	if _, err := webm.WriteTracks(ws, specs); err != nil {
		t.Fatal(err)
	}
	cw, err := webm.BeginCluster(ws, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.WriteSimpleBlock(1, 0, video, []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	end, _ := ws.Seek(0, 1)
	if err := webm.PatchSegmentSize(ws, h.SegmentSizeOff, 8, uint64(end-int64(segStart))); err != nil {
		t.Fatal(err)
	}
}

// keep time imported for potential future timing assertions.
var _ = time.Millisecond
