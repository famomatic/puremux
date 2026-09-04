package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

func TestProgressiveWriterRoundTripExactTiming(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "progressive-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := NewProgressiveWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	config := testAVCC()
	_, err = w.AddTrack(OutputTrack{ID: 7, Codec: core.CodecH264, TimeScale: 90000,
		Width: 1920, Height: 1080, ConfigType: "avcC", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	samples := []OutputSample{
		{TrackID: 7, DTS: 0, PTS: 3000, Duration: 3000, Keyframe: true, Data: []byte{0, 0, 0, 1, 0x65}},
		{TrackID: 7, DTS: 3000, PTS: 0, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}},
	}
	for _, sample := range samples {
		if err := w.WriteSample(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tracks := r.Tracks()
	if len(tracks) != 1 || tracks[0].ID != 7 || tracks[0].Timescale != 90000 ||
		tracks[0].Width != 1920 || tracks[0].Height != 1080 ||
		tracks[0].CodecConfigType != "avcC" || !bytes.Equal(tracks[0].CodecConfig, config) {
		t.Fatalf("track = %+v", tracks)
	}
	for i, want := range samples {
		got, err := r.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if got.DTS != want.DTS || got.PTS != want.PTS || got.Duration != want.Duration ||
			got.Keyframe != want.Keyframe || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("sample %d = %+v data=%x", i, got, got.Data)
		}
	}
	if _, err := r.NextSample(); err != io.EOF {
		t.Fatalf("tail error = %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	// ctts version 1 stores +3000 and -3000 as signed big-endian int32.
	if !bytes.Contains(data, []byte{'c', 't', 't', 's', 1, 0, 0, 0}) ||
		!bytes.Contains(data, []byte{0, 0, 0, 1, 0xff, 0xff, 0xf4, 0x48}) {
		t.Fatalf("missing signed ctts entries")
	}
}

func TestProgressiveWriterSampleBoundaries(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "boundary-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, _ := NewProgressiveWriter(f)
	_, err = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000,
		Channels: 2, SampleRate: 48000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSample(OutputSample{TrackID: 1, Duration: 0}); !errors.Is(err, ErrInvalidOutputSample) {
		t.Fatalf("zero duration error = %v", err)
	}
	if err := w.WriteSample(OutputSample{TrackID: 1, DTS: 0, PTS: 0, Duration: 1024, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSample(OutputSample{TrackID: 1, DTS: 2048, PTS: 2048, Duration: 1024, Data: []byte{2}}); !errors.Is(err, ErrInvalidOutputSample) {
		t.Fatalf("DTS gap error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestProgressiveWriterTimestampOverflowBoundaries(t *testing.T) {
	for _, sample := range []OutputSample{
		{TrackID: 1, DTS: math.MaxInt64, PTS: math.MaxInt64, Duration: 1, Data: []byte{1}},
		{TrackID: 1, DTS: math.MaxInt64, PTS: math.MinInt64, Duration: 1, Data: []byte{1}},
	} {
		f, err := os.CreateTemp(t.TempDir(), "overflow-*.mp4")
		if err != nil {
			t.Fatal(err)
		}
		w, _ := NewProgressiveWriter(f)
		_, err = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48_000,
			Channels: 2, SampleRate: 48_000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteSample(sample); !errors.Is(err, ErrInvalidOutputSample) {
			t.Fatalf("sample %+v error = %v", sample, err)
		}
		_ = f.Close()
	}
}
