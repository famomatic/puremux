package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type remuxTestDemuxer struct {
	streams []Stream
	packets []*Packet
	index   int
	format  Format
}

func (d *remuxTestDemuxer) Streams() []Stream { return cloneStreams(d.streams) }
func (d *remuxTestDemuxer) Info() Info        { return Info{Format: d.format} }
func (d *remuxTestDemuxer) ReadPacket(context.Context) (*Packet, error) {
	if d.index >= len(d.packets) {
		return nil, io.EOF
	}
	p := d.packets[d.index]
	d.index++
	return p, nil
}
func (d *remuxTestDemuxer) Seek(context.Context, SeekRequest) (SeekResult, error) {
	return SeekResult{}, ErrNotSeekable
}
func (d *remuxTestDemuxer) Close() error { return nil }

type remuxTestMuxer struct {
	streams []Stream
	data    [][]byte
	closed  int
	err     error
}

func (m *remuxTestMuxer) AddStream(stream Stream) (int, error) {
	m.streams = append(m.streams, stream)
	return len(m.streams) - 1, nil
}
func (m *remuxTestMuxer) WritePacket(_ context.Context, packet *Packet) error {
	if m.err != nil {
		return m.err
	}
	m.data = append(m.data, append([]byte(nil), packet.Data...))
	return nil
}
func (m *remuxTestMuxer) Close() error { m.closed++; return nil }

func TestRemuxDemuxersUsesExactDTSOrderingAndReleases(t *testing.T) {
	released := [2]int{}
	makePacket := func(id byte, releaseIndex int) *Packet {
		return &Packet{StreamIndex: 0, Data: []byte{id}, PTS: KnownTimestamp(1),
			DTS: KnownTimestamp(1), Duration: KnownTimestamp(1),
			release: func([]byte) { released[releaseIndex]++ }}
	}
	// 1/(2e9) second sorts after 1/(3e9) second. Both truncate to zero if
	// prematurely converted to nanoseconds, so this distinguishes exact
	// rational comparison from an approximate merge.
	late := &remuxTestDemuxer{streams: []Stream{{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 2_000_000_000}, SampleRate: 48_000, Channels: 2}},
		packets: []*Packet{makePacket('L', 0)}}
	early := &remuxTestDemuxer{streams: []Stream{{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 3_000_000_000}, SampleRate: 48_000, Channels: 2}},
		packets: []*Packet{makePacket('E', 1)}}
	output := &remuxTestMuxer{}
	if err := remuxDemuxers(context.Background(), []Demuxer{late, early}, output, FormatMP4); err != nil {
		t.Fatal(err)
	}
	if got := string(bytes.Join(output.data, nil)); got != "EL" {
		t.Fatalf("merge order = %q, want EL", got)
	}
	if released != [2]int{1, 1} || output.closed != 1 {
		t.Fatalf("released=%v closed=%d", released, output.closed)
	}
}

func TestRemuxDemuxersReleasesPrimedPacketsOnWriteError(t *testing.T) {
	released := 0
	packet := func(id byte) *Packet {
		return &Packet{StreamIndex: 0, Data: []byte{id}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0),
			Duration: KnownTimestamp(1), release: func([]byte) { released++ }}
	}
	stream := Stream{Type: MediaVideo, Codec: CodecVP8, TimeBase: Rational{Num: 1, Den: 1_000}, Width: 16, Height: 16}
	inputs := []Demuxer{
		&remuxTestDemuxer{streams: []Stream{stream}, packets: []*Packet{packet('a')}},
		&remuxTestDemuxer{streams: []Stream{stream}, packets: []*Packet{packet('b')}},
	}
	want := errors.New("write failed")
	output := &remuxTestMuxer{err: want}
	if err := remuxDemuxers(context.Background(), inputs, output, FormatWebM); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if released != 2 || output.closed != 1 {
		t.Fatalf("released=%d closed=%d", released, output.closed)
	}
}

func TestRemuxPublicWebMOpusToFragmentedMP4(t *testing.T) {
	input := makeRemuxWebM(t, []byte{0xf8, 0x55})
	var output bytes.Buffer
	err := Remux(context.Background(), []RemuxInput{{Source: MemorySource("input.webm", input)}},
		&output, MuxOptions{Format: FormatMP4})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("moof")) {
		t.Fatal("non-seekable Remux output is not fragmented MP4")
	}
	demuxer, err := Open(context.Background(), MemorySource("output.mp4", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	packet, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Data, []byte{0xf8, 0x55}) || packet.Duration.Value != 20_000_000 {
		t.Fatalf("packet = %+v data=%x", packet, packet.Data)
	}
}

func TestRemuxPublicMatroskaH264ToFragmentedMP4(t *testing.T) {
	var input bytes.Buffer
	muxer, err := NewMuxer(&input, MuxOptions{Format: FormatMatroska})
	if err != nil {
		t.Fatal(err)
	}
	avcc := []byte{1, 0x42, 0, 0x1f, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68}
	index, err := muxer.AddStream(Stream{Type: MediaVideo, Codec: CodecH264,
		TimeBase: Rational{Num: 1, Den: 1_000}, Width: 16, Height: 16,
		Config: CodecConfig{Format: CodecConfigAVCC, Data: avcc}})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 0, 0, 1, 0x65}
	if err := muxer.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: payload,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(40), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := muxer.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Remux(context.Background(), []RemuxInput{{Source: MemorySource("input.mkv", input.Bytes())}},
		&output, MuxOptions{Format: FormatMP4}); err != nil {
		t.Fatal(err)
	}
	demuxer, err := Open(context.Background(), MemorySource("output.mp4", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	streams := demuxer.Streams()
	if len(streams) != 1 || streams[0].Config.Format != CodecConfigAVCC || !bytes.Equal(streams[0].Config.Data, avcc) {
		t.Fatalf("streams = %+v", streams)
	}
	packet, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Data, payload) || packet.Duration.Value != 40_000_000 {
		t.Fatalf("packet = %+v data=%x", packet, packet.Data)
	}
}

func TestRemuxFilesInfersMP4AndInstallsAtomically(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.webm")
	outputPath := filepath.Join(dir, "output.mp4")
	if err := os.WriteFile(inputPath, makeRemuxWebM(t, []byte{0xf8, 0x55}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("old output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemuxFiles(context.Background(), []string{inputPath}, outputPath, MuxOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 || string(data[4:8]) != "ftyp" {
		t.Fatalf("output does not start with MP4 ftyp: %x", data)
	}
	if err := RemuxFiles(context.Background(), []string{inputPath}, inputPath, MuxOptions{Format: FormatMP4}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("aliased output = %v", err)
	}
	if _, err := outputFormatForPath("output.invalid"); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("invalid extension = %v", err)
	}
}

func TestRemuxRejectsIncompatibleMPEGTSFraming(t *testing.T) {
	demuxer := &remuxTestDemuxer{format: FormatMP4, streams: []Stream{{Type: MediaVideo,
		Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 90_000}, Width: 16, Height: 16}}}
	output := &remuxTestMuxer{}
	if err := remuxDemuxers(context.Background(), []Demuxer{demuxer}, output, FormatMPEGTS); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("framing error = %v", err)
	}
}

func makeRemuxWebM(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	muxer, err := NewMuxer(&buffer, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0, 0, 0)
	index, err := muxer.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: Rational{Num: 1, Den: 1_000}, SampleRate: 48_000, Channels: 2,
		Config: CodecConfig{Format: CodecConfigOpusHead, Data: head}})
	if err != nil {
		t.Fatal(err)
	}
	if err := muxer.WritePacket(context.Background(), &Packet{StreamIndex: index, Data: payload,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(20), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := muxer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buffer.Bytes()...)
}
