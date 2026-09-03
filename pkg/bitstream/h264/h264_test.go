package h264

import (
	"bytes"
	"testing"
)

func TestAVCCConfigurationAndConversion(t *testing.T) {
	// AVCDecoderConfigurationRecord: version 1, High profile, four-byte NAL
	// lengths (lengthSizeMinusOne=3), one SPS and one PPS. H.264 NAL headers
	// 0x67/0x68 are SPS/PPS and 0x65 is an IDR slice.
	record := []byte{1, 100, 0, 31, 0xff, 0xe1, 0, 2, 0x67, 0x64, 1, 0, 1, 0x68}
	config, err := ParseAVCC(record)
	if err != nil {
		t.Fatal(err)
	}
	packet := []byte{0, 0, 0, 2, 0x65, 0x88}
	annex, err := AVCCToAnnexB(config, packet, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 1, 0x67, 0x64, 0, 0, 0, 1, 0x68, 0, 0, 0, 1, 0x65, 0x88}
	if !bytes.Equal(annex, want) {
		t.Fatalf("Annex B = %x", annex)
	}
	if back, err := AnnexBToAVCC(annex[len(want)-6:], 4); err != nil || !bytes.Equal(back, packet) {
		t.Fatalf("back = %x, %v", back, err)
	}
}

func TestAVCCBoundaries(t *testing.T) {
	for _, data := range [][]byte{nil, {1, 100, 0, 31, 0xfe, 0xe1, 0}, {1, 100, 0, 31, 0xff, 0xe1, 0, 2, 0x67}} {
		if _, err := ParseAVCC(data); err == nil {
			t.Fatalf("malformed AVCC accepted: %x", data)
		}
	}
}
