package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/famomatic/puremux/internal/core"
	flacbits "github.com/famomatic/puremux/pkg/bitstream/flac"
)

func TestOutputBoxAndFullBoxExactBytes(t *testing.T) {
	box, err := outputBox("free", []byte{0xaa, 0xbb})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 10, 'f', 'r', 'e', 'e', 0xaa, 0xbb}
	if !bytes.Equal(box, want) {
		t.Fatalf("box = %x, want %x", box, want)
	}
	full, err := outputFullBox("test", 1, 0x020304, []byte{5})
	if err != nil {
		t.Fatal(err)
	}
	want = []byte{0, 0, 0, 13, 't', 'e', 's', 't', 1, 2, 3, 4, 5}
	if !bytes.Equal(full, want) {
		t.Fatalf("full box = %x, want %x", full, want)
	}
}

func TestVisualSampleEntryLayout(t *testing.T) {
	entry, err := sampleEntry(OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90000,
		Width: 1920, Height: 1080, ConfigType: "avcC", Config: testAVCC()})
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "avc1" || binary.BigEndian.Uint16(entry[32:34]) != 1920 ||
		binary.BigEndian.Uint16(entry[34:36]) != 1080 || string(entry[90:94]) != "avcC" {
		t.Fatalf("invalid visual sample entry: %x", entry)
	}
}

func testAVCC() []byte {
	// AVCDecoderConfigurationRecord with one one-byte SPS (NAL type 7) and
	// one one-byte PPS (NAL type 8), using four-byte NAL lengths.
	return []byte{1, 0x42, 0, 0x1f, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68}
}

func testHVCC() []byte {
	record := make([]byte, 23)
	record[0], record[13], record[15], record[16] = 1, 0xf0, 0xfc, 0xfc
	record[17], record[18], record[21], record[22] = 0xf8, 0xf8, 0xff, 3
	for _, pair := range []struct{ typ, nal byte }{{32, 0x40}, {33, 0x42}, {34, 0x44}} {
		record = append(record, 0x80|pair.typ, 0, 1, 0, 2, pair.nal, 1)
	}
	return record
}

func TestAACSampleEntryContainsSpecDerivedASC(t *testing.T) {
	// ISO/IEC 14496-3 AudioSpecificConfig: AAC-LC(2), 44.1kHz index 4,
	// stereo channelConfiguration 2, packed MSB-first => 0x12 0x10.
	entry, err := sampleEntry(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 44100,
		Channels: 2, SampleRate: 44100, ConfigType: "asc", Config: []byte{0x12, 0x10}})
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "mp4a" || !bytes.Contains(entry, []byte{0x05, 0x02, 0x12, 0x10}) {
		t.Fatalf("invalid AAC entry: %x", entry)
	}
}

func testDFLA(t *testing.T) []byte {
	t.Helper()
	streamInfo := make([]byte, 34)
	binary.BigEndian.PutUint16(streamInfo[0:2], 256)
	binary.BigEndian.PutUint16(streamInfo[2:4], 4096)
	// RFC 9639 MSB-first: 48 kHz, two channels, 16 bits/sample.
	binary.BigEndian.PutUint64(streamInfo[10:18], uint64(48000)<<44|uint64(1)<<41|uint64(15)<<36)
	payload, err := flacbits.DFLAPayload(streamInfo)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestAllSupportedCodecSampleEntries(t *testing.T) {
	// Each configuration is a hand-derived minimum valid record. Opus dOps
	// and FLAC dfLa use big-endian ISO BMFF fields; AV1/VP9 bit fields are
	// MSB-first, while AVC/HEVC NAL headers carry their specified unit types.
	tests := []struct {
		name      string
		track     OutputTrack
		entryType string
		childType string
	}{
		{"AVC", OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "avcC", Config: testAVCC()}, "avc1", "avcC"},
		{"HEVC", OutputTrack{ID: 1, Codec: core.CodecHEVC, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "hvcC", Config: testHVCC()}, "hvc1", "hvcC"},
		{"AV1", OutputTrack{ID: 1, Codec: core.CodecAV1, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "av1C", Config: []byte{0x81, 0, 0, 0}}, "av01", "av1C"},
		{"VP9", OutputTrack{ID: 1, Codec: core.CodecVP9, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "vpcC", Config: []byte{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 0}}, "vp09", "vpcC"},
		{"AAC", OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 44100, Channels: 2, SampleRate: 44100, ConfigType: "asc", Config: []byte{0x12, 0x10}}, "mp4a", "esds"},
		{"Opus", OutputTrack{ID: 1, Codec: core.CodecOpus, TimeScale: 48000, Channels: 2, SampleRate: 48000, ConfigType: "dOps", Config: []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 0}}, "Opus", "dOps"},
		{"FLAC", OutputTrack{ID: 1, Codec: core.CodecFLAC, TimeScale: 48000, Channels: 2, SampleRate: 48000, ConfigType: "dfLa", Config: testDFLA(t)}, "fLaC", "dfLa"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, err := sampleEntry(test.track)
			if err != nil {
				t.Fatal(err)
			}
			if string(entry[4:8]) != test.entryType || !bytes.Contains(entry, []byte(test.childType)) {
				t.Fatalf("entry type/config = %q/%q, bytes=%x", entry[4:8], test.childType, entry)
			}
		})
	}
}

func TestAV1BrandAndRequiredColourInformation(t *testing.T) {
	ftyp, err := makeFileTypeBox(false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ftyp, []byte("av01")) {
		t.Fatalf("AV1 compatible brand missing: %x", ftyp)
	}
	withoutAV1, err := makeFileTypeBox(false, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(withoutAV1, []byte("av01")) {
		t.Fatalf("unexpected AV1 compatible brand: %x", withoutAV1)
	}

	entry, err := sampleEntry(OutputTrack{ID: 1, Codec: core.CodecAV1, TimeScale: 90000,
		Width: 16, Height: 16, ConfigType: "av1C", Config: []byte{0x81, 0, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	// nclx uses big-endian CICP values 2/2/2 (unspecified), followed by
	// full_range_flag=0 and seven reserved zero bits.
	want := []byte{'c', 'o', 'l', 'r', 'n', 'c', 'l', 'x', 0, 2, 0, 2, 0, 2, 0}
	if !bytes.Contains(entry, want) {
		t.Fatalf("required AV1 nclx box missing: %x", entry)
	}
}

func TestSampleEntryBoundaries(t *testing.T) {
	badAVCType := testAVCC()
	badAVCType[8] = 0x68
	missingHEVCPPS := testHVCC()
	missingHEVCPPS[22] = 2
	missingHEVCPPS = missingHEVCPPS[:len(missingHEVCPPS)-7]
	badHEVCNALType := testHVCC()
	badHEVCNALType[28] = 0x42
	badAV1Reserved := []byte{0x81, 0, 0, 0x20}
	badAV1DelayReserved := []byte{0x81, 0, 0, 0x01}
	badAV1OBUOverrun := []byte{0x81, 0, 0, 0, 0x12, 1}
	badVP9Length := []byte{1, 0, 0, 0, 0, 10, 0, 1, 1, 1, 0, 1}
	cases := []OutputTrack{
		{},
		{ID: 1, Codec: core.CodecH264, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "hvcC", Config: []byte{1}},
		{ID: 1, Codec: core.CodecAAC, TimeScale: 44100, Channels: 0, SampleRate: 44100, ConfigType: "asc", Config: []byte{0x12, 0x10}},
		{ID: 1, Codec: core.CodecAAC, TimeScale: 44100, Channels: 2, SampleRate: 44100, ConfigType: "asc", Config: make([]byte, 65)},
		{ID: 1, Codec: core.CodecH264, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "avcC", Config: badAVCType},
		{ID: 1, Codec: core.CodecHEVC, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "hvcC", Config: missingHEVCPPS},
		{ID: 1, Codec: core.CodecHEVC, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "hvcC", Config: badHEVCNALType},
		{ID: 1, Codec: core.CodecAV1, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "av1C", Config: badAV1Reserved},
		{ID: 1, Codec: core.CodecAV1, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "av1C", Config: badAV1DelayReserved},
		{ID: 1, Codec: core.CodecAV1, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "av1C", Config: badAV1OBUOverrun},
		{ID: 1, Codec: core.CodecVP9, TimeScale: 90000, Width: 1, Height: 1, ConfigType: "vpcC", Config: badVP9Length},
	}
	for i, track := range cases {
		if _, err := sampleEntry(track); !errors.Is(err, ErrInvalidOutputTrack) {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteFullRejectsZeroProgress(t *testing.T) {
	if err := writeFull(zeroWriter{}, []byte{1}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want io.ErrShortWrite", err)
	}
}
