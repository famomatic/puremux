package hevc

import (
	"bytes"
	"testing"
)

func TestHVCCConfigurationAndConversion(t *testing.T) {
	// HEVCDecoderConfigurationRecord fixed header is 23 bytes. Byte 21 low
	// bits are lengthSizeMinusOne=3; array NAL types 32/33/34 are VPS/SPS/PPS.
	record := make([]byte, 23)
	record[0], record[21], record[22] = 1, 0xff, 3
	for _, pair := range []struct{ typ, nal byte }{{32, 0x40}, {33, 0x42}, {34, 0x44}} {
		record = append(record, 0x80|pair.typ, 0, 1, 0, 2, pair.nal, 1)
	}
	config, err := ParseHVCC(record)
	if err != nil {
		t.Fatal(err)
	}
	packet := []byte{0, 0, 0, 2, 0x26, 1} // HEVC IDR_W_RADL NAL type 19.
	annex, err := HVCCToAnnexB(config, packet, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(annex, []byte{0, 0, 0, 1, 0x26, 1}) || bytes.Count(annex, []byte{0, 0, 0, 1}) != 4 {
		t.Fatalf("Annex B = %x", annex)
	}
}

func TestHVCCBoundaries(t *testing.T) {
	reserved := make([]byte, 23)
	reserved[0], reserved[21] = 1, 2 // lengthSizeMinusOne=2 => reserved size 3.
	for _, data := range [][]byte{nil, reserved, append(append([]byte(nil), reserved[:21]...), 3, 1)} {
		if _, err := ParseHVCC(data); err == nil {
			t.Fatalf("malformed HVCC accepted: %x", data)
		}
	}
}
