package flac

import (
	"encoding/binary"
	"testing"
)

func testStreamInfo() []byte {
	data := make([]byte, 34)
	binary.BigEndian.PutUint16(data[0:2], 256)
	binary.BigEndian.PutUint16(data[2:4], 4096)
	// RFC 9639 STREAMINFO is packed MSB first: 20-bit rate, channels-1,
	// bits-per-sample-1, and 36-bit total sample count.
	packed := uint64(44100)<<44 | uint64(1)<<41 | uint64(15)<<36 | 123456
	binary.BigEndian.PutUint64(data[10:18], packed)
	return data
}

func TestParseStreamInfoSpecPacking(t *testing.T) {
	info, err := ParseStreamInfo(testStreamInfo())
	if err != nil {
		t.Fatal(err)
	}
	if info.MinBlockSize != 256 || info.MaxBlockSize != 4096 || info.SampleRate != 44100 || info.Channels != 2 || info.BitsPerSample != 16 || info.TotalSamples != 123456 {
		t.Fatalf("unexpected STREAMINFO: %+v", info)
	}
}

func TestParseFrameHeaderSpecFields(t *testing.T) {
	info, _ := ParseStreamInfo(testStreamInfo())
	// RFC 9639 frame header: 14-bit sync + fixed-block strategy, block-size
	// code 8 (256), sample-rate code 9 (44.1 kHz), independent stereo,
	// 16-bit samples, UTF-8-coded frame number zero, then CRC-8.
	header := []byte{0xff, 0xf8, 0x89, 0x18, 0x00}
	header = append(header, CRC8(header))
	parsed, err := ParseFrameHeader(header, info)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.VariableBlock || parsed.BlockSize != 256 || parsed.SampleRate != 44100 || parsed.Channels != 2 || parsed.BitsPerSample != 16 || parsed.Number != 0 || parsed.HeaderLength != 6 {
		t.Fatalf("unexpected frame header: %+v", parsed)
	}
}

func TestFLACHeaderBoundaries(t *testing.T) {
	if _, err := ParseStreamInfo(nil); err == nil {
		t.Fatal("nil STREAMINFO accepted")
	}
	badInfo := testStreamInfo()
	badInfo[0], badInfo[1] = 0, 0
	if _, err := ParseStreamInfo(badInfo); err == nil {
		t.Fatal("zero block size accepted")
	}
	info, _ := ParseStreamInfo(testStreamInfo())
	cases := [][]byte{
		nil,
		{0xff, 0xf8, 0x89, 0x18, 0x00},    // missing CRC
		{0xff, 0xfa, 0x89, 0x18, 0x00, 0}, // reserved sync bit
		{0xff, 0xf8, 0x09, 0x18, 0x00, 0}, // reserved block size
		{0xff, 0xf8, 0x8f, 0x18, 0x00, 0}, // reserved sample rate
		{0xff, 0xf8, 0x89, 0x1e, 0x00, 0}, // reserved sample size
		{0xff, 0xf8, 0x89, 0x18, 0x00, 0}, // wrong CRC
	}
	for i, data := range cases {
		if _, err := ParseFrameHeader(data, info); err == nil {
			t.Fatalf("case %d accepted malformed frame header", i)
		}
	}
}
