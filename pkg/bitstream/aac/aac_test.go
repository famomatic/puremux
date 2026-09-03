package aac

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestASCADTSRoundTrip(t *testing.T) {
	// MPEG-4 AudioSpecificConfig bits are MSB-first:
	// AOT=2 (AAC-LC), frequency_index=4 (44.1kHz), channel_config=2 -> 12 10.
	config, err := ParseASC([]byte{0x12, 0x10})
	if err != nil {
		t.Fatal(err)
	}
	if config.AudioObjectType != 2 || config.SampleRate != 44_100 || config.ChannelConfig != 2 {
		t.Fatalf("config = %+v", config)
	}
	if asc, err := ASC(config); err != nil || !bytes.Equal(asc, []byte{0x12, 0x10}) {
		t.Fatalf("ASC = %x, %v", asc, err)
	}
	raw := []byte{0x21, 0x10, 0x56}
	frame, err := WrapADTS(config, raw)
	if err != nil {
		t.Fatal(err)
	}
	// frame_length=10: high2=0, middle8=1, low3=2 -> bytes 3/4/5 fields.
	wantHeader := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x5f, 0xfc}
	if !bytes.Equal(frame[:7], wantHeader) {
		t.Fatalf("ADTS header = %x", frame[:7])
	}
	payload, parsed, err := StripADTS(frame)
	if err != nil || !bytes.Equal(payload, raw) || parsed != config {
		t.Fatalf("StripADTS = %x, %+v, %v", payload, parsed, err)
	}
}

func TestAACBoundaries(t *testing.T) {
	if _, err := ParseASC(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("empty ASC = %v", err)
	}
	if _, err := ParseASC([]byte{0x17, 0x00}); err == nil { // reserved frequency index 14.
		t.Fatal("reserved ASC frequency accepted")
	}
	for _, data := range [][]byte{nil, {0xff}, {0xff, 0xf1, 0x50, 0x80, 0xff, 0xff, 0xfc}} {
		if _, err := ParseADTS(data); err == nil {
			t.Fatalf("malformed ADTS accepted: %x", data)
		}
	}
	if _, err := WrapADTS(Config{AudioObjectType: 2, SampleRate: 44_100, FrequencyIndex: 4, ChannelConfig: 2}, make([]byte, 0x1fff)); err == nil {
		t.Fatal("oversized ADTS frame accepted")
	}
}
