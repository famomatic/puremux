package hevc

import (
	"bytes"
	"testing"
)

func testHVCCRecord() []byte {
	record := make([]byte, 23)
	// ISO/IEC 14496-15 reserved fields are fixed to one. Byte 21 contains
	// lengthSizeMinusOne=3; array types 32/33/34 are VPS/SPS/PPS.
	record[0], record[13], record[15], record[16] = 1, 0xf0, 0xfc, 0xfc
	record[17], record[18], record[21], record[22] = 0xf8, 0xf8, 0xff, 3
	for _, pair := range []struct{ typ, nal byte }{{32, 0x40}, {33, 0x42}, {34, 0x44}} {
		record = append(record, 0x80|pair.typ, 0, 1, 0, 2, pair.nal, 1)
	}
	return record
}

func TestHVCCConfigurationAndConversion(t *testing.T) {
	// HEVCDecoderConfigurationRecord fixed header is 23 bytes. Byte 21 low
	// bits are lengthSizeMinusOne=3; array NAL types 32/33/34 are VPS/SPS/PPS.
	record := testHVCCRecord()
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
	valid := testHVCCRecord()
	badLengthSize := append([]byte(nil), valid...)
	badLengthSize[21] = badLengthSize[21]&^3 | 2
	badFixedBits := append([]byte(nil), valid...)
	badFixedBits[13] &^= 0x10
	badArrayBit := append([]byte(nil), valid...)
	badArrayBit[23] |= 0x40
	trailing := append(append([]byte(nil), valid...), 0)
	for _, data := range [][]byte{nil, valid[:22], badLengthSize, badFixedBits, badArrayBit, trailing} {
		if _, err := ParseHVCC(data); err == nil {
			t.Fatalf("malformed HVCC accepted: %x", data)
		}
	}
}
