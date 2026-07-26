package core

import (
	"bytes"
	"testing"
	"time"
)

// adtsFrame builds a spec-accurate ADTS frame (MPEG-4, no CRC) by packing the
// header fields bit-by-bit per ISO/IEC 14496-3 §1.A.2.2. Every byte below is
// re-derivable by hand:
//
//	byte1 = 0xF0 | ID(0)<<3 | layer(00)<<1 | protection_absent(1) = 0xF1
//	byte2 = profile<<6 | freqIdx<<2 | private(0)<<1 | chanCfg>>2
//	byte3 = (chanCfg&3)<<6 | frameLen>>11
//	byte4 = (frameLen>>3)&0xFF
//	byte5 = (frameLen&7)<<5 | fullness>>6   (fullness=0x7FF → hi5=0x1F)
//	byte6 = (fullness&0x3F)<<2 | rawBlocks
func adtsFrame(profile, freqIdx, chanCfg byte, payload []byte) []byte {
	frameLen := adtsHeaderLen + len(payload)
	h := []byte{
		0xFF,
		0xF1,
		profile<<6 | freqIdx<<2 | chanCfg>>2,
		(chanCfg&0x03)<<6 | byte(frameLen>>11),
		byte(frameLen >> 3),
		byte(frameLen&0x07)<<5 | 0x1F,
		0xFC, // fullness lo6 = 0x3F -> 0xFC | rawBlocks(0)
	}
	return append(h, payload...)
}

func TestParseADTSHeader48k(t *testing.T) {
	// AAC-LC (profile bits 01), 48 kHz (index 3), stereo (config 2), 4-byte payload.
	// Hand-derived: byte2 = 0x40|0x0C|0 = 0x4C, byte3 = 0x80|0 = 0x80,
	// frameLen = 11 = 0b0000000001011 -> byte4 = 0x01, byte5 = 0x60|0x1F = 0x7F.
	fr := adtsFrame(1, 3, 2, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	want := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x7F, 0xFC}
	if !bytes.Equal(fr[:7], want) {
		t.Fatalf("header bytes = % X, want % X", fr[:7], want)
	}
	info, ok := ParseADTSHeader(fr)
	if !ok {
		t.Fatal("ParseADTSHeader failed")
	}
	if info.Length != 11 || info.SampleRate != 48000 || info.Channels != 2 || info.Samples != 1024 {
		t.Fatalf("info = %+v", info)
	}
	// 1024 samples at 48 kHz = 21.333...ms.
	if d := info.Duration(); d != 1024*time.Second/48000 {
		t.Fatalf("Duration = %v", d)
	}
}

func TestParseADTSHeader44k1(t *testing.T) {
	// 44.1 kHz is index 4: byte2 = 0x40|0x10 = 0x50.
	fr := adtsFrame(1, 4, 2, []byte{0x00})
	if fr[2] != 0x50 {
		t.Fatalf("byte2 = %#x, want 0x50", fr[2])
	}
	info, ok := ParseADTSHeader(fr)
	if !ok || info.SampleRate != 44100 {
		t.Fatalf("info = %+v ok=%v", info, ok)
	}
}

func TestParseADTSHeaderRejects(t *testing.T) {
	cases := map[string][]byte{
		"nil":            nil,
		"short":          {0xFF, 0xF1, 0x4C},
		"no sync":        {0x00, 0xF1, 0x4C, 0x80, 0x01, 0x7F, 0xFC},
		"bad sync lo":    {0xFF, 0x01, 0x4C, 0x80, 0x01, 0x7F, 0xFC},
		"layer nonzero":  {0xFF, 0xF7, 0x4C, 0x80, 0x01, 0x7F, 0xFC},
		"len < header":   {0xFF, 0xF1, 0x4C, 0x80, 0x00, 0x7F, 0xFC}, // frameLen=3
	}
	for name, b := range cases {
		if _, ok := ParseADTSHeader(b); ok {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseADTSHeaderReservedRate(t *testing.T) {
	// Reserved frequency index 13 -> SampleRate 0, Duration 0, still parses.
	fr := adtsFrame(1, 13, 2, []byte{0x00})
	info, ok := ParseADTSHeader(fr)
	if !ok {
		t.Fatal("reserved rate should still parse")
	}
	if info.SampleRate != 0 || info.Duration() != 0 {
		t.Fatalf("info = %+v", info)
	}
}

func TestForEachADTSFrameSplitsConcatenated(t *testing.T) {
	f1 := adtsFrame(1, 3, 2, []byte{0x11, 0x22})
	f2 := adtsFrame(1, 3, 2, []byte{0x33, 0x44, 0x55})
	var got [][]byte
	ForEachADTSFrame(append(append([]byte{}, f1...), f2...), func(fr []byte, info ADTSFrameInfo) bool {
		got = append(got, fr)
		return true
	})
	if len(got) != 2 || !bytes.Equal(got[0], f1) || !bytes.Equal(got[1], f2) {
		t.Fatalf("got %d frames", len(got))
	}
}

func TestForEachADTSFrameResyncAndTruncation(t *testing.T) {
	f1 := adtsFrame(1, 3, 2, []byte{0x11})
	// Garbage prefix, valid frame, then a truncated frame (header claims more
	// bytes than present) which must NOT be emitted.
	buf := append([]byte{0x00, 0x01, 0x02}, f1...)
	trunc := adtsFrame(1, 3, 2, []byte{0xAA, 0xBB, 0xCC})
	buf = append(buf, trunc[:len(trunc)-2]...)
	var got [][]byte
	ForEachADTSFrame(buf, func(fr []byte, info ADTSFrameInfo) bool {
		got = append(got, fr)
		return true
	})
	if len(got) != 1 || !bytes.Equal(got[0], f1) {
		t.Fatalf("got %d frames, want just f1", len(got))
	}
}

func TestForEachADTSFrameEarlyStop(t *testing.T) {
	f := adtsFrame(1, 3, 2, []byte{0x11})
	buf := append(append([]byte{}, f...), f...)
	n := 0
	ForEachADTSFrame(buf, func([]byte, ADTSFrameInfo) bool {
		n++
		return false
	})
	if n != 1 {
		t.Fatalf("early stop ignored, n=%d", n)
	}
}

func TestCodecAACIsAudio(t *testing.T) {
	if !CodecAAC.IsAudio() {
		t.Fatal("CodecAAC must report IsAudio")
	}
	if CodecAAC.IsVideo() {
		t.Fatal("CodecAAC must not report IsVideo")
	}
}
