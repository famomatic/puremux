package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

func TestFragmentedWriterRoundTripAndGOPCuts(t *testing.T) {
	var out bytes.Buffer
	w, err := NewFragmentedWriter(&out, 2*time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	config := testAVCC()
	_, err = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90000,
		Width: 1280, Height: 720, ConfigType: "avcC", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	samples := []OutputSample{
		{TrackID: 1, DTS: 0, PTS: 3000, Duration: 3000, Keyframe: true, Data: []byte{1, 2, 3}},
		{TrackID: 1, DTS: 3000, PTS: 0, Duration: 3000, Data: []byte{4, 5}},
		{TrackID: 1, DTS: 6000, PTS: 6000, Duration: 3000, Keyframe: true, Data: []byte{6, 7, 8, 9}},
	}
	for _, sample := range samples {
		if err := w.WriteSample(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(out.Bytes(), []byte{'m', 'o', 'o', 'f'}); count != 2 {
		t.Fatalf("moof count = %d, want 2", count)
	}
	if !bytes.Contains(out.Bytes(), []byte{'t', 'f', 'd', 't', 1, 0, 0, 0}) ||
		!bytes.Contains(out.Bytes(), []byte{'t', 'r', 'u', 'n', 1, 0, 0x0f, 1}) {
		t.Fatalf("missing v1 tfdt/trun")
	}
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
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
}

func TestFragmentedWriterTimestampOverflowBoundaries(t *testing.T) {
	for _, sample := range []OutputSample{
		{TrackID: 1, DTS: math.MaxInt64, PTS: math.MaxInt64, Duration: 1, Data: []byte{1}},
		{TrackID: 1, DTS: math.MaxInt64, PTS: math.MinInt64, Duration: 1, Data: []byte{1}},
	} {
		var output bytes.Buffer
		w, _ := NewFragmentedWriter(&output, 0, 0)
		_, err := w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48_000,
			Channels: 2, SampleRate: 48_000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteSample(sample); !errors.Is(err, ErrInvalidOutputSample) {
			t.Fatalf("sample %+v error = %v", sample, err)
		}
		if output.Len() != 0 {
			t.Fatalf("invalid sample wrote %d bytes", output.Len())
		}
	}
}

func TestFragmentedWriterAudioDurationCut(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewFragmentedWriter(&out, 40*time.Millisecond, 1024)
	_, err := w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000,
		Channels: 2, SampleRate: 48000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := w.WriteSample(OutputSample{TrackID: 1, DTS: int64(i * 1024), PTS: int64(i * 1024),
			Duration: 1024, Data: []byte{byte(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Count(out.Bytes(), []byte{'m', 'o', 'o', 'f'}) != 1 {
		t.Fatal("audio duration threshold did not flush")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFragmentedWriterBoundaries(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewFragmentedWriter(&out, 0, 2)
	_, err := w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000,
		Channels: 2, SampleRate: 48000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []OutputSample{
		{TrackID: 2, Duration: 1, Data: []byte{1}},
		{TrackID: 1, DTS: -1, PTS: -1, Duration: 1, Data: []byte{1}},
		{TrackID: 1, Duration: 1, Data: []byte{1, 2, 3}},
	} {
		if err := w.WriteSample(sample); !errors.Is(err, ErrInvalidOutputSample) {
			t.Fatalf("sample %+v error = %v", sample, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestFragmentedWriterBoundsTinyPacketMetadata(t *testing.T) {
	var out bytes.Buffer
	w, err := NewFragmentedWriter(&out, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// ISO ASC: MSB-first AAC-LC=2, sample-rate index=3 (48kHz), channel config=2.
	_, err = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000, Channels: 2, SampleRate: 48000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFragmentPackets+1; i++ {
		if err := w.WriteSample(OutputSample{TrackID: 1, DTS: int64(i), PTS: int64(i), Duration: 1, Data: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.pending) != 1 {
		t.Fatalf("unbounded metadata: %d", len(w.pending))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		sample, err := r.NextSample()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if sample.DTS != int64(count) || !bytes.Equal(sample.Data, []byte{1}) {
			t.Fatalf("sample %d: %+v", count, sample)
		}
		count++
	}
	if count != maxFragmentPackets+1 {
		t.Fatal(count)
	}
}
