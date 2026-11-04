package mp4

import "testing"

func TestSparseAudioSeekDirections(t *testing.T) {
	tr := &trackState{timescale: 48000, uniform: 1, stts: []sttsEntry{{count: 5000, delta: 1024}}, stsc: []stscEntry{{firstChunk: 1, samplesPerChunk: 5000}}, stco: []uint64{0}}
	for _, sample := range []uint32{0, 1023, 1024, 1025, 4096, 4999} {
		for _, backward := range []bool{true, false} {
			target := int64(sample)*1024 + 1
			want := sample
			if !backward && sample < 4999 {
				want++
			}
			index, pts, err := tr.findSeekIndexScaled(target, 48000, false, backward)
			if err != nil || index != want || pts != int64(want)*1024 {
				t.Fatalf("sample%d backward%v: %d %d %v", sample, backward, index, pts, err)
			}
		}
	}
	if len(tr.audioSeek) != 5 {
		t.Fatalf("checkpoints=%d", len(tr.audioSeek))
	}
}
