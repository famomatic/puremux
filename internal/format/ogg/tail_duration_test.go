package ogg

import (
	"bytes"
	"math"
	"testing"
)

// opusStream builds BOS OpusHead (pre-skip 312 = 0x0138, little-endian per
// RFC 7845 section 5.1), OpusTags and the given audio pages for serial 7.
func opusStream(audio ...[]byte) []byte {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	stream := append(makePage(0x02, 0, 7, 0, head), makePage(0, 0, 7, 1, opusTags("v"))...)
	for _, page := range audio {
		stream = append(stream, page...)
	}
	return stream
}

// RFC 6716 TOC 0xF8: config 31, code 0, one 20 ms (960-sample) CELT frame.
var frame = []byte{0xf8, 0x00}

func playbackDuration(t *testing.T, stream []byte, probe bool) (int64, bool) {
	t.Helper()
	r, err := NewPlaybackReaderWithTail(bytes.NewReader(stream), probe)
	if err != nil {
		t.Fatal(err)
	}
	return r.DurationSamples(), r.DurationKnown()
}

func TestPlaybackTailDuration(t *testing.T) {
	stream := opusStream(
		makePage(0, 312+960, 7, 2, frame),
		makePage(0x04, 312+3*960, 7, 3, frame, frame),
	)
	if d, ok := playbackDuration(t, stream, true); !ok || d != 3*960 {
		t.Fatalf("tail duration = %d, %v; want 2880", d, ok)
	}
	if _, ok := playbackDuration(t, stream, false); ok {
		t.Fatal("headers-only reader must not know the duration")
	}
	// Probing must leave the cursor at the start for playback.
	r, _ := NewPlaybackReaderWithTail(bytes.NewReader(stream), true)
	for i := 0; i < 3; i++ {
		if _, err := r.NextPacket(t.Context()); err != nil {
			t.Fatalf("packet %d after probe: %v", i, err)
		}
	}
}

func TestPlaybackTailDurationSkipsInvalidCandidates(t *testing.T) {
	good := makePage(0, 312+2*960, 7, 2, frame, frame)
	cases := map[string][]byte{
		// A page of another logical stream (serial 9) after ours.
		"other serial": opusStream(good, makePage(0, 99999, 9, 0, frame)),
		// Granule -1: no packet completes on the final page.
		"no granule": opusStream(good, makePage(0x04, math.MaxUint64, 7, 3, frame)),
		// Final page with a corrupted body byte fails its CRC.
		"bad crc": func() []byte {
			bad := makePage(0x04, 312+50*960, 7, 3, frame)
			bad[len(bad)-1] ^= 0xff
			return opusStream(good, bad)
		}(),
		// Final page truncated mid-body.
		"truncated": func() []byte {
			last := makePage(0x04, 312+50*960, 7, 3, frame, frame)
			return opusStream(good, last[:len(last)-1])
		}(),
	}
	for name, stream := range cases {
		if d, ok := playbackDuration(t, stream, true); !ok || d != 2*960 {
			t.Errorf("%s: duration = %d, %v; want 1920", name, d, ok)
		}
	}
	// Header pages only (granule 0 < pre-skip): duration stays unknown.
	if _, ok := playbackDuration(t, opusStream(), true); ok {
		t.Fatal("headers-only file reported a duration")
	}
}

func TestPlaybackTailDurationLargeFile(t *testing.T) {
	// 255 lacing values of 255 make a maximum-size page body (65025 bytes),
	// so the probe window starts inside an earlier page and must resync on
	// the final capture pattern.
	big := bytes.Repeat([]byte{0xf8}, 255*254)
	var pages [][]byte
	for i := 0; i < 4; i++ {
		pages = append(pages, makePage(0, uint64(312+(i+1)*960), 7, uint32(2+i), big))
	}
	stream := opusStream(pages...)
	if len(stream) <= maxOggPageSize {
		t.Fatalf("fixture too small: %d", len(stream))
	}
	if d, ok := playbackDuration(t, stream, true); !ok || d != 4*960 {
		t.Fatalf("large duration = %d, %v", d, ok)
	}
}
