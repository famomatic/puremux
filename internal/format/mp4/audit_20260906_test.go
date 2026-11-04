package mp4

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/famomatic/puremux/internal/core"
)

func audit20260906Track() OutputTrack {
	return OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000, SampleRate: 48000, Channels: 2, ConfigType: "asc", Config: []byte{0x11, 0x90}}
}
func TestAudit20260906TrunExpansion(t *testing.T) {
	// FullBox version=0, flags=0; sample_count u32 big endian 00 01 86 a0=100000.
	// All sample properties inherited from tfhd/trex; no per-sample fields needed.
	data := []byte{0, 0, 0, 0, 0, 1, 0x86, 0xa0}
	state := trafState{track: &trackState{timescale: 48000}, defaultValue: trackDefaults{duration: 1024, size: 1}}
	rd := &Reader{}
	err := rd.parseTrun(data, &state)
	if err == nil {
		t.Fatalf("8-byte trun expanded to %d samples, backing capacity %d bytes; uint32 count has no cap", len(rd.fragments), uintptr(cap(rd.fragments))*unsafe.Sizeof(fragmentSample{}))
	}
}
func TestAudit20260906FragmentPlaybackRetention(t *testing.T) {
	var out bytes.Buffer
	w, err := NewFragmentedWriter(&out, time.Nanosecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.AddTrack(audit20260906Track()); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 1000; i++ {
		if err = w.WriteSample(OutputSample{TrackID: 1, DTS: i * 1024, PTS: i * 1024, Duration: 1024, Data: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewPlaybackReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if _, err = r.NextSample(); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.fragments) > 1 {
		t.Fatalf("one-at-a-time playback retains %d completed fragment sample entries", len(r.fragments))
	}
}
func TestAudit20260906FailedProgressiveSeek(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "seek-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, _ := NewProgressiveWriter(f)
	_, err = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "avcC", Config: testAVCC()})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 3; i++ {
		if err = w.WriteSample(OutputSample{TrackID: 1, DTS: i * 3000, PTS: i * 3000, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, io.SeekStart)
	r, err := NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.NextSample(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.SeekTicksWithFlags(1, 3000, false, false); err == nil {
		t.Fatal("expected no sync sample")
	}
	s, err := r.NextSample()
	if err != nil {
		t.Fatalf("failed seek consumed remaining samples: %v", err)
	}
	if s.DTS != 3000 {
		t.Fatalf("resume DTS=%d want3000", s.DTS)
	}
}
func TestAudit20260906MissingChunkIsNotEOF(t *testing.T) {
	// stsz/stts sample_count=2, stsc one sample/chunk, only one stco entry.
	track := &trackState{timescale: 48000, uniform: 1, stts: []sttsEntry{{count: 2, delta: 1024}}, stsc: []stscEntry{{firstChunk: 1, samplesPerChunk: 1}}, stco: []uint64{0}}
	r := &Reader{rs: bytes.NewReader([]byte{1, 2}), tracks: []*trackState{track}}
	if _, err := r.NextSample(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextSample(); err == io.EOF {
		t.Fatal("inconsistent sample table silently becomes EOF")
	}
}

func TestTrunCountAndFieldBoundaries(t *testing.T) {
	// ISO BMFF FullBox flags0, sample_count65536 = 00 01 00 00, big-endian.
	for _, tc := range []struct {
		data []byte
		ok   bool
	}{
		{[]byte{0, 0, 0, 0, 0, 1, 0, 0}, true},
		{[]byte{0, 0, 0, 0, 0, 1, 0, 1}, false},
		{nil, false}, {[]byte{0, 0, 0, 0, 0, 0, 0}, false},
		{[]byte{2, 0, 0, 0, 0, 0, 0, 0}, false},
		{[]byte{0, 0, 1, 0, 0, 0, 0, 1}, false},
		{[]byte{0, 0, 4, 4, 0, 0, 0, 0, 0, 0, 0, 0}, false},
	} {
		rd := &Reader{}
		state := trafState{track: &trackState{timescale: 48000}, defaultValue: trackDefaults{duration: 1024, size: 1}}
		err := rd.parseTrun(tc.data, &state)
		if (err == nil) != tc.ok {
			t.Fatalf("data %x err %v", tc.data, err)
		}
	}
}
func FuzzBoundedTrun(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 1})
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0x86, 0xa0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		rd := &Reader{}
		state := trafState{track: &trackState{timescale: 48000}, defaultValue: trackDefaults{duration: 1024, size: 1}}
		_ = rd.parseTrun(data, &state)
	})
}

func TestMultipleValidDescriptionsRejected(t *testing.T) {
	entry, err := sampleEntry(audit20260906Track())
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{0, 0, 0, 2}, entry...)
	payload = append(payload, entry...)
	data, err := outputFullBox("stsd", 0, 0, payload)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)
	box, err := readBox(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = (&Reader{}).parseStsd(r, box, &trackState{}); err == nil {
		t.Fatal("two valid descriptions silently accepted")
	}
}
func TestSamplePayloadLimitPrecedesAllocation(t *testing.T) {
	tr := &trackState{timescale: 48000, uniform: maxInputSampleBytes + 1, stts: []sttsEntry{{count: 1, delta: 1024}}, stsc: []stscEntry{{firstChunk: 1, samplesPerChunk: 1}}, stco: []uint64{0}}
	rd := &Reader{rs: bytes.NewReader(nil), tracks: []*trackState{tr}}
	if _, err := rd.NextSample(); err != ErrCorrupt {
		t.Fatalf("oversized declaration=%v", err)
	}
}
