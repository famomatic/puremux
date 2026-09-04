package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/famomatic/puremux/pkg/bitstream/aac"
	flacbits "github.com/famomatic/puremux/pkg/bitstream/flac"
)

func TestOpenADTSRawPacketsAndSeek(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	first, _ := aac.WrapADTS(config, []byte{1, 2, 3})
	second, _ := aac.WrapADTS(config, []byte{4, 5})
	d, err := Open(context.Background(), MemorySource("audio.aac", append(first, second...)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stream := d.Streams()[0]
	if d.Info().Format != FormatADTS || stream.Codec != CodecAAC || stream.SampleRate != 44100 || stream.Channels != 2 || stream.Config.Format != CodecConfigASC || !bytes.Equal(stream.Config.Data, []byte{0x12, 0x10}) {
		t.Fatalf("unexpected ADTS stream: %+v", stream)
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(p.Data, []byte{1, 2, 3}) || p.PTS.Value != 0 || p.Duration.Value != 1024 {
		t.Fatalf("first packet=%+v err=%v", p, err)
	}
	result, err := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 1024})
	if err != nil || result.Timestamp != 1024 {
		t.Fatalf("seek=%+v err=%v", result, err)
	}
	p, err = d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(p.Data, []byte{4, 5}) || p.PTS.Value != 1024 {
		t.Fatalf("second packet=%+v err=%v", p, err)
	}
}

func TestSeekFlagsChooseDirectionAndRejectUnknownBits(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	first, _ := aac.WrapADTS(config, []byte{1})
	second, _ := aac.WrapADTS(config, []byte{2})
	d, err := Open(context.Background(), MemorySource("seek.aac", append(first, second...)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	forward, err := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 512})
	if err != nil || forward.Timestamp != 1024 {
		t.Fatalf("forward seek = %+v, %v; want 1024", forward, err)
	}
	backward, err := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 512, Flags: SeekBackward})
	if err != nil || backward.Timestamp != 0 {
		t.Fatalf("backward seek = %+v, %v; want 0", backward, err)
	}
	if _, err := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Flags: SeekFlags(0x80)}); err == nil {
		t.Fatal("unknown seek flag was silently accepted")
	}
}

func TestOpenMP3WithID3Metadata(t *testing.T) {
	frameHeader := []byte{0xff, 0xfb, 0x90, 0x64}
	frame := append(append([]byte(nil), frameHeader...), make([]byte, 417-4)...)
	title := append([]byte{3}, []byte("Spec title")...)
	tagFrame := make([]byte, 10+len(title))
	copy(tagFrame, "TIT2")
	binary.BigEndian.PutUint32(tagFrame[4:8], uint32(len(title)))
	copy(tagFrame[10:], title)
	id3 := make([]byte, 10)
	copy(id3, "ID3")
	id3[3] = 3
	id3[9] = byte(len(tagFrame)) // syncsafe because the fixture is <128 bytes.
	data := append(append(id3, tagFrame...), append(frame, frame...)...)
	d, err := Open(context.Background(), MemorySource("audio.mp3", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stream := d.Streams()[0]
	if d.Info().Format != FormatMP3 || stream.Codec != CodecMP3 || stream.SampleRate != 44100 || stream.Channels != 2 || stream.Metadata["title"] != "Spec title" {
		t.Fatalf("unexpected MP3 stream: %+v", stream)
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil || len(p.Data) != 417 || p.Duration.Value != 1152 || p.Pos != int64(len(id3)+len(tagFrame)) {
		t.Fatalf("packet=%+v err=%v", p, err)
	}
}

func TestOpenFLACFramesAndComments(t *testing.T) {
	streamInfo := make([]byte, 34)
	binary.BigEndian.PutUint16(streamInfo[0:2], 256)
	binary.BigEndian.PutUint16(streamInfo[2:4], 256)
	packed := uint64(44100)<<44 | uint64(1)<<41 | uint64(15)<<36 | 512
	binary.BigEndian.PutUint64(streamInfo[10:18], packed)
	comments := vorbisCommentFixture("reference", "TITLE=FLAC title")
	data := append([]byte("fLaC"), metadataBlock(0, false, streamInfo)...)
	data = append(data, metadataBlock(4, true, comments)...)
	data = append(data, flacFrameFixture(0, 0x11)...)
	data = append(data, flacFrameFixture(1, 0x22)...)
	d, err := Open(context.Background(), MemorySource("audio.flac", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stream := d.Streams()[0]
	if d.Info().Format != FormatFLAC || stream.Codec != CodecFLAC || stream.SampleRate != 44100 || stream.Channels != 2 || stream.Metadata["title"] != "FLAC title" || stream.Config.Format != CodecConfigFLACStreamInfo {
		t.Fatalf("unexpected FLAC stream: %+v", stream)
	}
	for i, want := range []byte{0x11, 0x22} {
		p, err := d.ReadPacket(context.Background())
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if p.PTS.Value != int64(i*256) || p.Duration.Value != 256 || p.Data[len(p.Data)-1] != want {
			t.Fatalf("packet %d: %+v", i, p)
		}
	}
	if _, err := d.ReadPacket(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("end read=%v", err)
	}
}

func TestOpenRawMalformedBoundaries(t *testing.T) {
	for name, data := range map[string][]byte{
		"adts truncated":          {0xff, 0xf1, 0x50, 0x80},
		"mp3 overrun":             append([]byte{0xff, 0xfb, 0x90, 0x64}, make([]byte, 8)...),
		"flac truncated metadata": append([]byte("fLaC"), 0x80, 0, 0, 34),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(context.Background(), MemorySource(name, data), OpenOptions{}); err == nil {
				t.Fatal("malformed stream accepted")
			}
		})
	}
}

func metadataBlock(typ byte, last bool, payload []byte) []byte {
	header := []byte{typ, byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	if last {
		header[0] |= 0x80
	}
	return append(header, payload...)
}

func vorbisCommentFixture(vendor string, comments ...string) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(vendor)))
	out.WriteString(vendor)
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(comments)))
	for _, comment := range comments {
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(comment)))
		out.WriteString(comment)
	}
	return out.Bytes()
}

func flacFrameFixture(number byte, marker byte) []byte {
	// RFC 9639 fixed-block frame: block code 8=256 samples, rate code
	// 9=44.1 kHz, independent stereo, 16-bit, one-byte UTF-8 frame number.
	header := []byte{0xff, 0xf8, 0x89, 0x18, number}
	header = append(header, flacbits.CRC8(header))
	return append(header, 0x00, 0x00, marker)
}
