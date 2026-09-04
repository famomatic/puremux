package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
)

func TestMP4MuxerProgressivePublicRoundTrip(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "public-progressive-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mux, err := NewMuxer(f, MuxOptions{Format: FormatMP4})
	if err != nil {
		t.Fatal(err)
	}
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0, 0, 0)
	stream, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0xf8, 0x55}
	packet := &Packet{StreamIndex: stream, Data: payload,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(960), Flags: PacketKeyframe}
	if err := mux.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	payload[1] = 0xaa
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	demux, err := Open(context.Background(), MemorySource("roundtrip.mp4", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demux.Close()
	streams := demux.Streams()
	if len(streams) != 1 || streams[0].Codec != CodecOpus || streams[0].Config.Format != CodecConfigDOPS ||
		streams[0].TimeBase != (Rational{Num: 1, Den: 48_000}) {
		t.Fatalf("stream = %+v", streams)
	}
	got, err := demux.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PTS.Value != 0 || got.DTS.Value != 0 || got.Duration.Value != 960 ||
		!bytes.Equal(got.Data, []byte{0xf8, 0x55}) {
		t.Fatalf("packet = %+v data=%x", got, got.Data)
	}
}

func TestMP4MuxerFragmentedAutoNonSeekable(t *testing.T) {
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatMP4})
	if err != nil {
		t.Fatal(err)
	}
	index, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigASC, Data: []byte{0x11, 0x90}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: []byte{0x11, 0x22},
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1024), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("moof")) || !bytes.Contains(output.Bytes(), []byte("mdat")) {
		t.Fatalf("auto non-seekable output is not fragmented MP4")
	}
	demux, err := Open(context.Background(), MemorySource("fragmented.mp4", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demux.Close()
	got, err := demux.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Duration.Value != 1024 || !bytes.Equal(got.Data, []byte{0x11, 0x22}) {
		t.Fatalf("packet = %+v data=%x", got, got.Data)
	}
}

func TestWebMMuxerPublicRoundTripAndStreamingHeader(t *testing.T) {
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0, 0, 0)
	index, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0xf8, 0x55} // RFC 6716 TOC config 31/code 0: one 20 ms frame.
	if err := mux.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: payload,
		PTS: KnownTimestamp(10), DTS: KnownTimestamp(10), Duration: KnownTimestamp(20), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	payload[1] = 0xaa
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	// RFC 8794 unknown-size VINT width 8: marker plus all value bits set.
	if !bytes.Contains(output.Bytes(), []byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatal("streaming Segment does not contain the width-8 unknown-size sentinel")
	}
	demux, err := Open(context.Background(), MemorySource("roundtrip.webm", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demux.Close()
	streams := demux.Streams()
	if len(streams) != 1 || streams[0].CodecDelay != 6_500_000 || streams[0].SeekPreRoll != 80_000_000 {
		t.Fatalf("Opus timing metadata = %+v", streams)
	}
	got, err := demux.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PTS.Value != 10_000_000 || got.DTS.Value != 10_000_000 || got.Duration.Value != 20_000_000 ||
		!bytes.Equal(got.Data, []byte{0xf8, 0x55}) {
		t.Fatalf("packet = %+v data=%x", got, got.Data)
	}
}

func TestWebMOpusZeroPreSkipWritesCodecDelay(t *testing.T) {
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7845 OpusHead is little-endian: pre-skip=0, input rate=48,000,
	// output gain=0, mapping family=0.
	head := append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	index, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(context.Background(), &Packet{StreamIndex: index,
		Data: []byte{0xf8}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0),
		Duration: KnownTimestamp(960), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte{0x56, 0xaa, 0x81, 0x00}) {
		t.Fatalf("zero Opus CodecDelay missing from WebM: %x", output.Bytes())
	}
}

func TestEBMLOpusStreamValidation(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	tests := []struct {
		name   string
		stream Stream
		want   error
	}{
		{name: "missing head", stream: Stream{Type: MediaAudio, Codec: CodecOpus,
			TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2}, want: ErrUnsupportedCodec},
		{name: "truncated head", stream: Stream{Type: MediaAudio, Codec: CodecOpus,
			TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2,
			Config: CodecConfig{Format: CodecConfigOpusHead, Data: head[:18]}}, want: ErrInvalidData},
		{name: "channel mismatch", stream: Stream{Type: MediaAudio, Codec: CodecOpus,
			TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 1,
			Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}}, want: ErrInvalidData},
		{name: "delay mismatch", stream: Stream{Type: MediaAudio, Codec: CodecOpus,
			TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2, CodecDelay: 1,
			Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}}, want: ErrInvalidData},
		{name: "negative preroll", stream: Stream{Type: MediaAudio, Codec: CodecOpus,
			TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2, SeekPreRoll: -1,
			Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}}, want: ErrInvalidData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux, err := NewMuxer(&bytes.Buffer{}, MuxOptions{Format: FormatWebM})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mux.AddStream(test.stream); !errors.Is(err, test.want) {
				t.Fatalf("AddStream error = %v, want %v", err, test.want)
			}
		})
	}
}

func ebmlTestAVCC() []byte {
	// AVCDecoderConfigurationRecord: reserved fields are ones, lengthSize=4,
	// and the MSB-first NAL headers identify SPS type 7 and PPS type 8.
	return []byte{1, 0x42, 0, 0x1f, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68}
}

func ebmlTestHVCC() []byte {
	record := make([]byte, 23)
	record[0], record[13], record[15], record[16] = 1, 0xf0, 0xfc, 0xfc
	record[17], record[18], record[21], record[22] = 0xf8, 0xf8, 0xff, 3
	// VPS/SPS/PPS types 32/33/34 occupy bits 6:1 of the first NAL byte;
	// byte 1 low bits encode nuh_temporal_id_plus1=1.
	for _, pair := range []struct{ typ, nal byte }{{32, 0x40}, {33, 0x42}, {34, 0x44}} {
		record = append(record, 0x80|pair.typ, 0, 1, 0, 2, pair.nal, 1)
	}
	return record
}

func ebmlTestFLACStreamInfo() []byte {
	streamInfo := make([]byte, 34)
	binary.BigEndian.PutUint16(streamInfo[0:2], 256)
	binary.BigEndian.PutUint16(streamInfo[2:4], 4096)
	// RFC 9639 STREAMINFO fields are MSB-first: 48 kHz, channel count minus
	// one = 1 (stereo), and bits-per-sample minus one = 15 (16-bit).
	binary.BigEndian.PutUint64(streamInfo[10:18], uint64(48_000)<<44|uint64(1)<<41|uint64(15)<<36)
	return streamInfo
}

func ebmlTestVorbisHeaders() []byte {
	identification := make([]byte, 30)
	identification[0] = 1
	copy(identification[1:7], "vorbis")
	identification[11] = 2
	binary.LittleEndian.PutUint32(identification[12:16], 48_000)
	// The identification header's low/high nibbles encode 2^6 and 2^10
	// sample block sizes; the final framing bit is one.
	identification[28], identification[29] = 0xa6, 1
	comment := append([]byte{3}, []byte("vorbis")...)
	comment = append(comment, make([]byte, 8)...)
	comment = append(comment, 1)
	setup := append([]byte{5}, []byte("vorbis")...)
	setup = append(setup, 0)
	// Matroska/Xiph lacing stores packet-count-minus-one followed by the
	// first two packet lengths; both lengths fit in one byte here.
	private := []byte{2, byte(len(identification)), byte(len(comment))}
	private = append(private, identification...)
	private = append(private, comment...)
	return append(private, setup...)
}

func TestEBMLMandatoryCodecPrivateValidation(t *testing.T) {
	video := func(codec CodecID, format CodecConfigFormat, data []byte) Stream {
		return Stream{Type: MediaVideo, Codec: codec, TimeBase: Rational{Num: 1, Den: 1_000},
			Width: 16, Height: 16, Config: CodecConfig{Format: format, Data: data}}
	}
	audio := func(codec CodecID, format CodecConfigFormat, data []byte) Stream {
		return Stream{Type: MediaAudio, Codec: codec, TimeBase: Rational{Num: 1, Den: 48_000},
			SampleRate: 48_000, Channels: 2, Config: CodecConfig{Format: format, Data: data}}
	}
	validAVC := ebmlTestAVCC()
	validHEVC := ebmlTestHVCC()
	validAV1 := []byte{0x81, 0, 0, 0}
	validFLAC := ebmlTestFLACStreamInfo()
	validFLACChain := append([]byte("fLaC"), 0, 0, 0, 34)
	validFLACChain = append(validFLACChain, validFLAC...)
	validFLACChain = append(validFLACChain, 0x81, 0, 0, 0)
	validVorbis := ebmlTestVorbisHeaders()

	valid := []struct {
		name       string
		stream     Stream
		privateLen int
	}{
		{name: "AVC", stream: video(CodecH264, CodecConfigAVCC, validAVC), privateLen: len(validAVC)},
		{name: "HEVC", stream: video(CodecHEVC, CodecConfigHVCC, validHEVC), privateLen: len(validHEVC)},
		{name: "AV1", stream: video(CodecAV1, CodecConfigAV1C, validAV1), privateLen: len(validAV1)},
		{name: "FLAC raw STREAMINFO", stream: audio(CodecFLAC, CodecConfigFLACStreamInfo, validFLAC), privateLen: 42},
		{name: "FLAC metadata chain", stream: audio(CodecFLAC, CodecConfigFLACStreamInfo, validFLACChain), privateLen: len(validFLACChain)},
		{name: "Vorbis", stream: audio(CodecVorbis, CodecConfigVorbisHeaders, validVorbis), privateLen: len(validVorbis)},
	}
	for _, test := range valid {
		t.Run("valid "+test.name, func(t *testing.T) {
			mux, err := NewMuxer(&bytes.Buffer{}, MuxOptions{Format: FormatMatroska})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mux.AddStream(test.stream); err != nil {
				t.Fatalf("AddStream: %v", err)
			}
			if got := len(mux.(*ebmlMuxer).tracks[0].CodecPrivate); got != test.privateLen {
				t.Fatalf("CodecPrivate length = %d, want %d", got, test.privateLen)
			}
		})
	}

	badAVCType := append([]byte(nil), validAVC...)
	badAVCType[8] = 0x68
	badHEVCTemporalID := append([]byte(nil), validHEVC...)
	badHEVCTemporalID[29] = 0
	badAV1Reserved := []byte{0x81, 0, 0, 0x20}
	badFLACRate := append([]byte(nil), validFLAC...)
	binary.BigEndian.PutUint64(badFLACRate[10:18], uint64(44_100)<<44|uint64(1)<<41|uint64(15)<<36)
	badVorbisLacing := []byte{2, 255}
	invalid := []struct {
		name   string
		stream Stream
		want   error
	}{
		{name: "AVC missing", stream: video(CodecH264, CodecConfigUnknown, nil), want: ErrUnsupportedCodec},
		{name: "AVC truncated", stream: video(CodecH264, CodecConfigAVCC, []byte{1}), want: ErrInvalidData},
		{name: "AVC wrong parameter type", stream: video(CodecH264, CodecConfigAVCC, badAVCType), want: ErrInvalidData},
		{name: "HEVC missing", stream: video(CodecHEVC, CodecConfigUnknown, nil), want: ErrUnsupportedCodec},
		{name: "HEVC forbidden temporal id", stream: video(CodecHEVC, CodecConfigHVCC, badHEVCTemporalID), want: ErrInvalidData},
		{name: "AV1 missing", stream: video(CodecAV1, CodecConfigUnknown, nil), want: ErrUnsupportedCodec},
		{name: "AV1 truncated", stream: video(CodecAV1, CodecConfigAV1C, validAV1[:3]), want: ErrInvalidData},
		{name: "AV1 reserved bit", stream: video(CodecAV1, CodecConfigAV1C, badAV1Reserved), want: ErrInvalidData},
		{name: "FLAC missing", stream: audio(CodecFLAC, CodecConfigUnknown, nil), want: ErrUnsupportedCodec},
		{name: "FLAC truncated", stream: audio(CodecFLAC, CodecConfigFLACStreamInfo, validFLAC[:33]), want: ErrInvalidData},
		{name: "FLAC property mismatch", stream: audio(CodecFLAC, CodecConfigFLACStreamInfo, badFLACRate), want: ErrInvalidData},
		{name: "Vorbis missing", stream: audio(CodecVorbis, CodecConfigUnknown, nil), want: ErrUnsupportedCodec},
		{name: "Vorbis lacing overrun", stream: audio(CodecVorbis, CodecConfigVorbisHeaders, badVorbisLacing), want: ErrInvalidData},
		{name: "Vorbis truncated setup", stream: audio(CodecVorbis, CodecConfigVorbisHeaders, validVorbis[:len(validVorbis)-2]), want: ErrInvalidData},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			mux, err := NewMuxer(&bytes.Buffer{}, MuxOptions{Format: FormatMatroska})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mux.AddStream(test.stream); !errors.Is(err, test.want) {
				t.Fatalf("AddStream error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestVP9CodecConfigurationRepresentationConversion(t *testing.T) {
	// VP9 MP4 vpcC byte 6 is MSB-first: 8-bit depth (1000), chroma 1
	// (001), limited range (0) => 1000_0010b = 0x82.
	vpcc := []byte{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 0}
	features := []byte{1, 1, 0, 2, 1, 10, 3, 1, 8, 4, 1, 1}
	stream := Stream{Type: MediaVideo, Codec: CodecVP9,
		Config: CodecConfig{Format: CodecConfigVPCC, Data: vpcc}}
	private, err := normalizeEBMLCodecPrivate(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(private, features) {
		t.Fatalf("Matroska VP9 features = %x, want %x", private, features)
	}

	stream.Config = CodecConfig{Format: CodecConfigVP9FeatureMetadata, Data: features}
	typ, converted, err := normalizeMP4Config(stream)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "vpcC" || !bytes.Equal(converted, vpcc) {
		t.Fatalf("MP4 VP9 config = %q/%x, want vpcC/%x", typ, converted, vpcc)
	}

	stream.Config.Data = []byte{1, 1, 0, 2, 1, 10, 3, 1, 8}
	if _, _, err := normalizeMP4Config(stream); err == nil {
		t.Fatal("incomplete Matroska VP9 feature list converted to MP4")
	}
}

func TestMatroskaFLACMetadataChainConvertsToMP4(t *testing.T) {
	streamInfo := ebmlTestFLACStreamInfo()
	// RFC 9639: non-final STREAMINFO (00 00 00 22), followed by a final
	// zero-length padding block (81 00 00 00), all fields MSB-first.
	chain := append([]byte("fLaC"), 0, 0, 0, 34)
	chain = append(chain, streamInfo...)
	chain = append(chain, 0x81, 0, 0, 0)
	stream := Stream{Type: MediaAudio, Codec: CodecFLAC,
		Config: CodecConfig{Format: CodecConfigFLACStreamInfo, Data: chain}}
	typ, got, err := normalizeMP4Config(stream)
	if err != nil {
		t.Fatal(err)
	}
	want, err := flac.DFLAPayload(streamInfo)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "dfLa" || !bytes.Equal(got, want) {
		t.Fatalf("MP4 FLAC config = %q/%x, want dfLa/%x", typ, got, want)
	}

	nonFinal := append([]byte("fLaC"), 0, 0, 0, 34)
	nonFinal = append(nonFinal, streamInfo...)
	stream.Config.Data = nonFinal
	if _, _, err := normalizeMP4Config(stream); err == nil {
		t.Fatal("non-final Matroska FLAC chain converted to MP4")
	}
}

func TestEBMLVorbisCodecPrivateRoundTrip(t *testing.T) {
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	private := ebmlTestVorbisHeaders()
	index, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecVorbis,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigVorbisHeaders, Data: private}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: []byte{0},
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(960), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	demux, err := Open(context.Background(), MemorySource("vorbis.webm", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demux.Close()
	streams := demux.Streams()
	if len(streams) != 1 || streams[0].Config.Format != CodecConfigVorbisHeaders ||
		!bytes.Equal(streams[0].Config.Data, private) {
		t.Fatalf("Vorbis stream config = %+v", streams)
	}
}

func TestMPEGTSMuxerPublicRoundTrip(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48_000, FrequencyIndex: 3, ChannelConfig: 2}
	frame, err := aac.WrapADTS(config, []byte{0x11, 0x22, 0x33})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	index, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigASC, Data: []byte{0x11, 0x90}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: frame,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1024), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Len()%188 != 0 || output.Bytes()[0] != 0x47 {
		t.Fatalf("invalid TS framing: size=%d", output.Len())
	}
	demux, err := Open(context.Background(), MemorySource("roundtrip.ts", output.Bytes()), OpenOptions{FormatHint: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	defer demux.Close()
	got, err := demux.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, []byte{0x11, 0x22, 0x33}) {
		t.Fatalf("packet data = %x", got.Data)
	}
}

func TestMuxerBackendBoundaries(t *testing.T) {
	for _, format := range []Format{FormatWebM, FormatMatroska, FormatMPEGTS} {
		var output bytes.Buffer
		mux, err := NewMuxer(&output, MuxOptions{Format: format})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if _, err := mux.AddStream(Stream{Type: MediaAudio, Codec: CodecMP3,
			TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2}); !errors.Is(err, ErrUnsupportedCodec) {
			t.Fatalf("%s unsupported codec = %v", format, err)
		}
	}
	var output bytes.Buffer
	mux, err := NewMuxer(&output, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	index, err := mux.AddStream(Stream{Type: MediaVideo, Codec: CodecVP8,
		TimeBase: Rational{Num: 1, Den: 1_000}, Width: 16, Height: 16})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Packet{StreamIndex: index, Data: []byte{0}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1)}
	if err := mux.WritePacket(canceled, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	p.Duration.Valid = false
	if err := mux.WritePacket(context.Background(), p); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("missing duration = %v", err)
	}
	if _, err := mux.AddStream(Stream{}); err == nil {
		t.Fatal("AddStream with invalid stream unexpectedly succeeded")
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v", err)
	}
	if err := mux.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	var tsOutput bytes.Buffer
	tsMuxer, err := NewMuxer(&tsOutput, MuxOptions{Format: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	tsIndex, err := tsMuxer.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := tsMuxer.WritePacket(context.Background(), &Packet{StreamIndex: tsIndex,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(0)}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("zero TS duration = %v", err)
	}
	if tsOutput.Len() != 0 {
		t.Fatalf("zero-duration TS packet wrote %d bytes", tsOutput.Len())
	}
}
