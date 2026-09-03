package mp3

import "testing"

func TestParseHeaderSpecFields(t *testing.T) {
	// ISO/IEC 11172-3 header fields, MSB first: sync=0x7ff, MPEG-1,
	// Layer III, no CRC, bitrate index 9 (128 kb/s), frequency index 0
	// (44.1 kHz), joint stereo. floor(144*128000/44100) = 417 bytes.
	h, err := ParseHeader([]byte{0xff, 0xfb, 0x90, 0x64})
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 1 || h.Layer != 3 || h.BitRate != 128000 || h.SampleRate != 44100 || h.Channels != 2 || h.FrameLength != 417 || h.Samples != 1152 || h.CRCProtected {
		t.Fatalf("unexpected MPEG-1 Layer III header: %+v", h)
	}

	// MPEG-2 Layer III, 64 kb/s (index 8), 24 kHz (index 1), mono,
	// with padding: floor(72*64000/24000)+1 = 193 bytes, 576 samples.
	h, err = ParseHeader([]byte{0xff, 0xf3, 0x86, 0xc0})
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 2 || h.Layer != 3 || h.BitRate != 64000 || h.SampleRate != 24000 || h.Channels != 1 || h.FrameLength != 193 || h.Samples != 576 || !h.Padding {
		t.Fatalf("unexpected MPEG-2 Layer III header: %+v", h)
	}
}

func TestParseHeaderBoundaries(t *testing.T) {
	cases := [][]byte{
		nil,
		{0xff, 0xfb, 0x90},       // truncated
		{0x00, 0xfb, 0x90, 0x64}, // bad sync
		{0xff, 0xeb, 0x90, 0x64}, // reserved MPEG version
		{0xff, 0xf9, 0x90, 0x64}, // reserved layer
		{0xff, 0xfb, 0x00, 0x64}, // free-format bitrate
		{0xff, 0xfb, 0xf0, 0x64}, // reserved bitrate
		{0xff, 0xfb, 0x9c, 0x64}, // reserved frequency
		{0xff, 0xfb, 0x90, 0x66}, // reserved emphasis
	}
	for i, data := range cases {
		if _, err := ParseHeader(data); err == nil {
			t.Fatalf("case %d accepted malformed header", i)
		}
	}
}
