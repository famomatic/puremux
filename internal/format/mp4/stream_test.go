package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// These tests exercise the streaming cursor paths that the original
// resolveTrack also covered, but against multi-chunk / multi-stts inputs that
// stress the O(1) cursor advance (chunk transitions, stts entry transitions).

func TestStreamingMultiChunk(t *testing.T) {
	// 3 samples, each in its own chunk (3 chunks). stsc says 1 sample/chunk.
	sizes := []uint32{3, 3, 3}
	stts := []sttsEntry{{count: 3, delta: 10}} // 0,10,20 ms
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 1}}
	stss := []uint32{1}
	mdat := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	// First build with 3 placeholder offsets so moov size matches the final
	// build (stco entry count, not values, determines moov length).
	trak := buildTrak("vp09", 1000, stts, sizes, stsc, []uint32{0, 0, 0}, stss)
	data, off := buildMP4([][]byte{trak}, mdat)
	stco := []uint32{uint32(off), uint32(off) + 3, uint32(off) + 6}
	trak = buildTrak("vp09", 1000, stts, sizes, stsc, stco, stss)
	data, _ = buildMP4([][]byte{trak}, mdat)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		ms   uint64
		data []byte
		key  bool
	}{
		{0, []byte{1, 2, 3}, true},
		{10, []byte{4, 5, 6}, false},
		{20, []byte{7, 8, 9}, false},
	}
	for i, w := range want {
		s, err := rd.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if s.AbsMs != w.ms {
			t.Errorf("sample %d ms = %d want %d", i, s.AbsMs, w.ms)
		}
		if s.Keyframe != w.key {
			t.Errorf("sample %d key = %v want %v", i, s.Keyframe, w.key)
		}
		if !bytes.Equal(s.Data, w.data) {
			t.Errorf("sample %d data = %v want %v", i, s.Data, w.data)
		}
	}
	if _, err := rd.NextSample(); err == nil {
		t.Error("expected EOF after 3 samples")
	}
}

func TestStreamingMultiSTTSEntries(t *testing.T) {
	// 3 samples with varying deltas across 2 stts entries: [1x delta=5, 2x delta=20].
	// Times: 0, 5, 25 ms.
	sizes := []uint32{2, 2, 2}
	stts := []sttsEntry{{count: 1, delta: 5}, {count: 2, delta: 20}}
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 3}}
	stss := []uint32{1}
	mdat := []byte{0xA1, 0xA2, 0xB1, 0xB2, 0xC1, 0xC2}
	trak := buildTrak("vp09", 1000, stts, sizes, stsc, nil, stss)
	data, off := buildMP4([][]byte{trak}, mdat)
	trak = buildTrak("vp09", 1000, stts, sizes, stsc, []uint32{uint32(off)}, stss)
	data, _ = buildMP4([][]byte{trak}, mdat)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	wantMs := []uint64{0, 5, 25}
	for i, w := range wantMs {
		s, err := rd.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if s.AbsMs != w {
			t.Errorf("sample %d ms = %d want %d", i, s.AbsMs, w)
		}
	}
}

func TestStreamingAudioNoStss(t *testing.T) {
	// Audio track: no stss. Keyframe must be false for every sample.
	sizes := []uint32{4, 4}
	stts := []sttsEntry{{count: 2, delta: 20}}
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 2}}
	mdat := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	trak := buildTrak("Opus", 1000, stts, sizes, stsc, nil, nil)
	data, off := buildMP4([][]byte{trak}, mdat)
	trak = buildTrak("Opus", 1000, stts, sizes, stsc, []uint32{uint32(off)}, nil)
	data, _ = buildMP4([][]byte{trak}, mdat)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := rd.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if s.Keyframe {
			t.Errorf("audio sample %d should not be keyframe", i)
		}
	}
}

// TestStreamingUniformSize verifies the uniform-size (stsz uniform != 0) path.
func TestStreamingUniformSize(t *testing.T) {
	// uniform size 4, 3 samples, timescale 1000, delta 33.
	stts := []sttsEntry{{count: 3, delta: 33}}
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 3}}
	stss := []uint32{1}
	mdat := []byte{1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3}
	// Build stsz with uniform=4 (count 3).
	stszChildren := bytes.Join([][]byte{
		fullBox("stsd", stsdPayload("vp09")),
		fullBox("stts", sttsPayload(stts)),
		fullBox("stsz", uniformStszPayload(4, 3)),
		fullBox("stsc", stscPayload(stsc)),
		fullBox("stco", stcoPayload([]uint32{0})),
		fullBox("stss", stssPayload(stss)),
	}, nil)
	stbl := mkBox("stbl", stszChildren)
	minf := mkBox("minf", stbl)
	mdhd := fullBox("mdhd", mdhdPayload(1000))
	mdia := mkBox("mdia", bytes.Join([][]byte{mdhd, minf}, nil))
	tkhd := fullBox("tkhd", tkhdPayload())
	trak := mkBox("trak", bytes.Join([][]byte{tkhd, mdia}, nil))
	data, off := buildMP4([][]byte{trak}, mdat)
	// Rebuild with the real stco offset.
	stszChildren = bytes.Join([][]byte{
		fullBox("stsd", stsdPayload("vp09")),
		fullBox("stts", sttsPayload(stts)),
		fullBox("stsz", uniformStszPayload(4, 3)),
		fullBox("stsc", stscPayload(stsc)),
		fullBox("stco", stcoPayload([]uint32{uint32(off)})),
		fullBox("stss", stssPayload(stss)),
	}, nil)
	stbl = mkBox("stbl", stszChildren)
	minf = mkBox("minf", stbl)
	mdia = mkBox("mdia", bytes.Join([][]byte{mdhd, minf}, nil))
	trak = mkBox("trak", bytes.Join([][]byte{tkhd, mdia}, nil))
	data, _ = buildMP4([][]byte{trak}, mdat)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	wantData := [][]byte{{1, 1, 1, 1}, {2, 2, 2, 2}, {3, 3, 3, 3}}
	for i, w := range wantData {
		s, err := rd.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if len(s.Data) != 4 {
			t.Errorf("sample %d size = %d want 4 (uniform)", i, len(s.Data))
		}
		if !bytes.Equal(s.Data, w) {
			t.Errorf("sample %d data = %v want %v", i, s.Data, w)
		}
	}
	_ = fmt.Sprintf
}

// uniformStszPayload builds an stsz payload with a uniform size and count,
// WITHOUT per-sample entries (version/flags added by fullBox).
func uniformStszPayload(uniform, count uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[0:4], uniform)
	binary.BigEndian.PutUint32(p[4:8], count)
	return p
}
