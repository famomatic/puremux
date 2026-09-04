package flac

import (
	"bytes"
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

func TestDFLAPayloadRoundTrip(t *testing.T) {
	streamInfo := testStreamInfo()
	payload, err := DFLAPayload(streamInfo)
	if err != nil {
		t.Fatal(err)
	}
	// ISO BMFF dfLa is a version-0 FullBox followed by the FLAC metadata
	// block header: last=1, type=STREAMINFO(0), 24-bit length=34.
	wantPrefix := []byte{0, 0, 0, 0, 0x80, 0, 0, 34}
	if !bytes.Equal(payload[:8], wantPrefix) {
		t.Fatalf("dfLa prefix = %x, want %x", payload[:8], wantPrefix)
	}
	got, err := StreamInfoFromDFLA(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, streamInfo) {
		t.Fatal("STREAMINFO changed during dfLa conversion")
	}
	for _, malformed := range [][]byte{nil, payload[:41], append([]byte(nil), payload...)} {
		if len(malformed) == len(payload) {
			malformed[4] = 0
		}
		if _, err := StreamInfoFromDFLA(malformed); err == nil {
			t.Fatalf("malformed dfLa accepted: %x", malformed)
		}
	}
}

func TestMatroskaCodecPrivateMetadataChain(t *testing.T) {
	streamInfo := testStreamInfo()
	minimal, info, err := MatroskaCodecPrivate(streamInfo)
	if err != nil || len(minimal) != 42 || string(minimal[:4]) != "fLaC" || minimal[4] != 0x80 ||
		info.SampleRate != 44_100 || info.Channels != 2 {
		t.Fatalf("minimal = %x info=%+v err=%v", minimal, info, err)
	}
	// RFC 9639 metadata headers are MSB-first: the first STREAMINFO header
	// has last=0/type=0/length=34, followed by a final type-1 padding block
	// with a zero 24-bit length.
	chain := append([]byte("fLaC"), 0, 0, 0, 34)
	chain = append(chain, streamInfo...)
	chain = append(chain, 0x81, 0, 0, 0)
	got, _, err := MatroskaCodecPrivate(chain)
	if err != nil || !bytes.Equal(got, chain) {
		t.Fatalf("metadata chain = %x, err=%v", got, err)
	}
	dfla, err := DFLAFromMatroskaCodecPrivate(chain)
	if err != nil {
		t.Fatal(err)
	}
	wantDFLA, err := DFLAPayload(streamInfo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dfla, wantDFLA) {
		t.Fatalf("dfLa = %x, want %x", dfla, wantDFLA)
	}

	sizeOverrun := append([]byte(nil), chain...)
	sizeOverrun[len(sizeOverrun)-1] = 1
	duplicateStreamInfo := append([]byte(nil), chain...)
	duplicateStreamInfo[len(duplicateStreamInfo)-4] = 0x80
	nonFinalMinimal := append([]byte("fLaC"), 0, 0, 0, 34)
	nonFinalMinimal = append(nonFinalMinimal, streamInfo...)
	for name, malformed := range map[string][]byte{
		"nil":                  nil,
		"truncated block":      chain[:len(chain)-1],
		"size overrun":         sizeOverrun,
		"duplicate STREAMINFO": duplicateStreamInfo,
		"non-final minimal":    nonFinalMinimal,
		"data after final":     append(append([]byte(nil), minimal...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := MatroskaCodecPrivate(malformed); err == nil {
				t.Fatal("malformed Matroska FLAC initialization accepted")
			}
			if _, err := DFLAFromMatroskaCodecPrivate(malformed); err == nil {
				t.Fatal("malformed Matroska FLAC initialization converted to dfLa")
			}
		})
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
	badInfo[0], badInfo[1] = 0, 15
	if _, err := ParseStreamInfo(badInfo); err == nil {
		t.Fatal("block size below RFC 9639 minimum accepted")
	}
	badInfo = testStreamInfo()
	packed := binary.BigEndian.Uint64(badInfo[10:18])
	packed = packed&^(uint64(0x1f)<<36) | uint64(2)<<36 // stored 2 means 3 bits/sample.
	binary.BigEndian.PutUint64(badInfo[10:18], packed)
	if _, err := ParseStreamInfo(badInfo); err == nil {
		t.Fatal("bit depth below RFC 9639 minimum accepted")
	}
	info, _ := ParseStreamInfo(testStreamInfo())
	// Block-size code 7 stores (block size - 1) as a 16-bit value. RFC 9639
	// forbids stored 0xffff because the resulting 65536 exceeds STREAMINFO.
	forbiddenBlockSize := []byte{0xff, 0xf8, 0x70, 0x18, 0x00, 0xff, 0xff}
	forbiddenBlockSize = append(forbiddenBlockSize, CRC8(forbiddenBlockSize))
	cases := [][]byte{
		nil,
		{0xff, 0xf8, 0x89, 0x18, 0x00},    // missing CRC
		{0xff, 0xfa, 0x89, 0x18, 0x00, 0}, // reserved sync bit
		{0xff, 0xf8, 0x09, 0x18, 0x00, 0}, // reserved block size
		{0xff, 0xf8, 0x8f, 0x18, 0x00, 0}, // reserved sample rate
		{0xff, 0xf8, 0x89, 0x1e, 0x00, 0}, // reserved sample size
		{0xff, 0xf8, 0x89, 0x18, 0x00, 0}, // wrong CRC
		forbiddenBlockSize,
	}
	for i, data := range cases {
		if _, err := ParseFrameHeader(data, info); err == nil {
			t.Fatalf("case %d accepted malformed frame header", i)
		}
	}
}
