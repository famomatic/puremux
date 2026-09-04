package h264

import (
	"bytes"
	"testing"
)

func TestAVCCConfigurationAndConversion(t *testing.T) {
	// AVCDecoderConfigurationRecord: version 1, High profile, four-byte NAL
	// lengths (lengthSizeMinusOne=3), one SPS and one PPS. H.264 NAL headers
	// 0x67/0x68 are SPS/PPS and 0x65 is an IDR slice.
	record := []byte{1, 100, 0, 31, 0xff, 0xe1, 0, 2, 0x67, 0x64, 1, 0, 1, 0x68,
		0xfd, 0xf8, 0xf8, 0}
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
	for _, data := range [][]byte{
		nil,
		{1, 100, 0, 31, 0xfe, 0xe1, 0},          // reserved lengthSizeMinusOne value
		{1, 100, 0, 31, 0x03, 0xe1, 0},          // byte 4 reserved bits are not all one
		{1, 100, 0, 31, 0xff, 0x01, 0},          // byte 5 reserved bits are not all one
		{1, 100, 0, 31, 0xff, 0xe1, 0, 2, 0x67}, // SPS overruns record
		// High profile requires the chroma/bit-depth/SPS-ext fields after PPS.
		{1, 100, 0, 31, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68},
		// Baseline profile has no extension, so trailing bytes are malformed.
		{1, 66, 0, 31, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68, 0},
	} {
		if _, err := ParseAVCC(data); err == nil {
			t.Fatalf("malformed AVCC accepted: %x", data)
		}
	}
}

func TestValidateAVCCParameterSetExtension(t *testing.T) {
	// High Profile extension fields are MSB-first: reserved six/five/five
	// bits are one; chroma_format=1 and both bit-depth-minus-eight fields=0.
	record := []byte{1, 100, 0, 31, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68,
		0xfd, 0xf8, 0xf8, 1, 0, 1, 0x6d}
	if err := ValidateAVCC(record); err != nil {
		t.Fatalf("valid High Profile extension rejected: %v", err)
	}
	badType := append([]byte(nil), record...)
	badType[len(badType)-1] = 0x67
	if err := ValidateAVCC(badType); err == nil {
		t.Fatal("wrong SPS extension NAL type accepted")
	}
}
