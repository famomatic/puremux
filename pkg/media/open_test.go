package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/mpegts"
	oggformat "github.com/famomatic/puremux/internal/format/ogg"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
)

func TestOpenOfficialLibwebmFixture(t *testing.T) {
	encoded, err := os.ReadFile("../../internal/format/webm/testdata/libwebm_discard_padding.webm.b64")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	demuxer, err := Open(context.Background(), MemorySource("discard_padding.webm", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	if info := demuxer.Info(); info.Format != FormatWebM || info.FormatName != "webm" {
		t.Fatalf("unexpected info: %+v", info)
	}
	streams := demuxer.Streams()
	if len(streams) != 1 || streams[0].Codec != CodecOpus || streams[0].Channels != 2 {
		t.Fatalf("unexpected streams: %+v", streams)
	}
	wantPadding := []int64{12_810_000, 127, -128}
	for i, want := range wantPadding {
		packet, err := demuxer.ReadPacket(context.Background())
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if got := int64(packet.DiscardPadding); got != want {
			t.Fatalf("packet %d padding = %d, want %d", i, got, want)
		}
		if !packet.PTS.Valid || packet.PTS.Value != 0 || packet.Duration.Value != 10_000_000 {
			t.Fatalf("packet %d timestamps: %+v duration=%+v", i, packet.PTS, packet.Duration)
		}
		packet.Release()
	}
	if _, err := demuxer.ReadPacket(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadPacket after fixture = %v, want EOF", err)
	}
	result, err := demuxer.Seek(context.Background(), SeekRequest{StreamIndex: -1, Target: 0})
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if result.Timestamp != 0 {
		t.Fatalf("Seek timestamp = %d, want 0", result.Timestamp)
	}
	if _, err := demuxer.ReadPacket(context.Background()); err != nil {
		t.Fatalf("ReadPacket after Seek: %v", err)
	}
}

func TestOpenOggOpus(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	tags := mediaTestOpusTags("vendor", "TITLE=Ogg title")
	audio := []byte{0xf8, 0x55} // RFC 6716 config 31/code 0: 960 samples.
	data := append(mediaTestOggPage(0x02, 0, 1, 0, head), mediaTestOggPage(0, 0, 1, 1, tags)...)
	data = append(data, mediaTestOggPage(0x04, 312+960, 1, 2, audio)...)
	demuxer, err := Open(context.Background(), MemorySource("audio.opus", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	streams := demuxer.Streams()
	if len(streams) != 1 || streams[0].Codec != CodecOpus || streams[0].TimeBase != (Rational{Num: 1, Den: 48_000}) || streams[0].Metadata["title"] != "Ogg title" {
		t.Fatalf("unexpected streams: %+v", streams)
	}
	p, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PTS.Value != 0 || p.Duration.Value != 960 || !bytes.Equal(p.Data, audio) {
		t.Fatalf("unexpected packet: %+v", p)
	}
}

func TestOpenProgressiveMP4Opus(t *testing.T) {
	data := mediaTestMP4Opus(0)
	mdatOffset := len(data) - 2
	data = mediaTestMP4Opus(uint32(mdatOffset))
	demuxer, err := Open(context.Background(), MemorySource("audio.m4a", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	streams := demuxer.Streams()
	if len(streams) != 1 || streams[0].Codec != CodecOpus || streams[0].TimeBase != (Rational{Num: 1, Den: 48_000}) || streams[0].Config.Format != CodecConfigDOPS {
		t.Fatalf("unexpected MP4 stream: %+v", streams)
	}
	p, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PTS.Value != 0 || p.DTS.Value != 0 || p.Duration.Value != 960 || !bytes.Equal(p.Data, []byte{0xf8, 0x01}) {
		t.Fatalf("unexpected MP4 packet: %+v", p)
	}
	if _, err := demuxer.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 0}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBoundariesAndSeek(t *testing.T) {
	if _, err := Open(context.Background(), MemorySource("short", []byte{0x1a}), OpenOptions{}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("short input = %v", err)
	}
	if _, err := Open(context.Background(), MemorySource("unknown", []byte("nope")), OpenOptions{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("unknown input = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(canceled, MemorySource("canceled", []byte("nope")), OpenOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Open = %v", err)
	}
}

func TestOpenHonorsMaxProbeBytes(t *testing.T) {
	source := &countingSeekSource{Reader: bytes.NewReader([]byte("not-a-format"))}
	if _, err := Open(context.Background(), source, OpenOptions{MaxProbeBytes: 4}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Open error = %v, want ErrUnsupportedFormat", err)
	}
	if source.bytesRead != 4 {
		t.Fatalf("probe read %d bytes, want exactly 4", source.bytesRead)
	}
	for _, limit := range []int64{-1, 1, 3} {
		source = &countingSeekSource{Reader: bytes.NewReader([]byte("not-a-format"))}
		if _, err := Open(context.Background(), source, OpenOptions{MaxProbeBytes: limit}); err == nil {
			t.Fatalf("MaxProbeBytes=%d unexpectedly succeeded", limit)
		}
		if source.bytesRead != 0 {
			t.Fatalf("MaxProbeBytes=%d read %d bytes before validation", limit, source.bytesRead)
		}
	}
}

func TestOpenSequentialMPEGTSWithHint(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48_000, FrequencyIndex: 3, ChannelConfig: 2}
	frame, err := aac.WrapADTS(config, []byte{0x11, 0x22, 0x33})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	mux := mpegts.New(&encoded)
	track, err := mux.AddTrack(core.Track{ID: 7, Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := mux.WritePacket(&core.Packet{TrackID: track, Codec: core.CodecAAC, Data: frame, PTS: time.Second + time.Duration(i)*time.Second, DTS: time.Second + time.Duration(i)*time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	for _, opts := range []OpenOptions{{FormatHint: FormatMPEGTS}, {ProbeSequential: true}} {
		source := &sequentialTestSource{reader: bytes.NewReader(encoded.Bytes())}
		demuxer, err := Open(context.Background(), source, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer demuxer.Close()
		if source.bytesRead >= len(encoded.Bytes()) {
			t.Fatalf("streaming Open consumed all %d bytes instead of returning after the first completed PES", source.bytesRead)
		}
		if _, err := demuxer.Seek(context.Background(), SeekRequest{StreamIndex: -1}); !errors.Is(err, ErrNotSeekable) {
			t.Fatalf("streaming seek = %v, want ErrNotSeekable", err)
		}
		packet, err := demuxer.ReadPacket(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(packet.Data, []byte{0x11, 0x22, 0x33}) {
			t.Fatalf("packet data = % X", packet.Data)
		}
	}
}

func TestStreamingMPEGTSCloseUnblocksPacketRead(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48_000, FrequencyIndex: 3, ChannelConfig: 2}
	frame, _ := aac.WrapADTS(config, []byte{1})
	var encoded bytes.Buffer
	mux := mpegts.New(&encoded)
	track, err := mux.AddTrack(core.Track{ID: 1, Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := mux.WritePacket(&core.Packet{TrackID: track, Codec: core.CodecAAC, Data: frame, PTS: time.Duration(i) * time.Second, DTS: time.Duration(i) * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	source := &blockingContextSource{data: encoded.Bytes(), blockAfter: 3, blocked: make(chan struct{}), closed: make(chan struct{})}
	demuxer, err := Open(context.Background(), source, OpenOptions{FormatHint: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := demuxer.ReadPacket(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := demuxer.ReadPacket(context.Background())
		done <- err
	}()
	select {
	case <-source.blocked:
	case <-time.After(time.Second):
		t.Fatal("packet read did not reach the streaming source")
	}
	if err := demuxer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("read unblocked with %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock packet read")
	}
}

func TestOpenSequentialRequiresHintWithoutConsumingSource(t *testing.T) {
	source := &sequentialTestSource{reader: bytes.NewReader([]byte{0x47, 0, 0, 0})}
	if _, err := Open(context.Background(), source, OpenOptions{}); !errors.Is(err, ErrNotSeekable) {
		t.Fatalf("Open error = %v, want ErrNotSeekable", err)
	}
	if source.bytesRead != 0 {
		t.Fatalf("Open consumed %d bytes before rejecting missing hint", source.bytesRead)
	}
}

type countingSeekSource struct {
	*bytes.Reader
	bytesRead int
}

func (s *countingSeekSource) Name() string { return "counting" }
func (s *countingSeekSource) Close() error { return nil }
func (s *countingSeekSource) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.bytesRead += n
	return n, err
}

type sequentialTestSource struct {
	reader    *bytes.Reader
	bytesRead int
}

type blockingContextSource struct {
	data       []byte
	reads      int
	blockAfter int
	blocked    chan struct{}
	closed     chan struct{}
}

func (s *blockingContextSource) Name() string { return "blocking.ts" }
func (s *blockingContextSource) Read(p []byte) (int, error) {
	return s.ReadContext(context.Background(), p)
}
func (s *blockingContextSource) ReadContext(ctx context.Context, p []byte) (int, error) {
	if s.reads >= s.blockAfter {
		select {
		case <-s.blocked:
		default:
			close(s.blocked)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-s.closed:
			return 0, ErrClosed
		}
	}
	start := s.reads * 188
	if start >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[start:min(start+188, len(s.data))])
	s.reads++
	return n, nil
}
func (s *blockingContextSource) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func (s *sequentialTestSource) Name() string { return "sequential" }
func (s *sequentialTestSource) Close() error { return nil }
func (s *sequentialTestSource) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	s.bytesRead += n
	return n, err
}

func mediaTestOggPage(flags byte, granule uint64, serial, sequence uint32, packets ...[]byte) []byte {
	var lacing, body []byte
	for _, packet := range packets {
		lacing = append(lacing, byte(len(packet)))
		body = append(body, packet...)
	}
	page := make([]byte, 27+len(lacing)+len(body))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	copy(page[27+len(lacing):], body)
	binary.LittleEndian.PutUint32(page[22:26], oggformat.CRC(page))
	return page
}

func mediaTestOpusTags(vendor string, comments ...string) []byte {
	var buf bytes.Buffer
	buf.WriteString("OpusTags")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(vendor)))
	buf.WriteString(vendor)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(comments)))
	for _, comment := range comments {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(comment)))
		buf.WriteString(comment)
	}
	return buf.Bytes()
}

func mediaTestMP4Opus(chunkOffset uint32) []byte {
	box := func(typ string, payload []byte) []byte {
		out := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(out, uint32(len(out)))
		copy(out[4:8], typ)
		copy(out[8:], payload)
		return out
	}
	full := func(typ string, payload []byte) []byte { return box(typ, append(make([]byte, 4), payload...)) }
	u32row := func(values ...uint32) []byte {
		out := make([]byte, 4*len(values))
		for i, value := range values {
			binary.BigEndian.PutUint32(out[i*4:], value)
		}
		return out
	}
	ftyp := box("ftyp", append([]byte("isom\x00\x00\x02\x00"), []byte("isom")...))
	dops := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 0}
	audioBody := make([]byte, 28)
	binary.BigEndian.PutUint16(audioBody[6:8], 1)
	binary.BigEndian.PutUint16(audioBody[16:18], 2)
	binary.BigEndian.PutUint16(audioBody[18:20], 16)
	binary.BigEndian.PutUint32(audioBody[24:28], 48_000<<16)
	audioBody = append(audioBody, box("dOps", dops)...)
	entry := append(make([]byte, 8), audioBody...)
	binary.BigEndian.PutUint32(entry, uint32(len(entry)))
	copy(entry[4:8], "Opus")
	stsd := full("stsd", append(u32row(1), entry...))
	stts := full("stts", u32row(1, 1, 960))
	stsz := full("stsz", u32row(0, 1, 2))
	stsc := full("stsc", u32row(1, 1, 1, 1))
	stco := full("stco", u32row(1, chunkOffset))
	stbl := box("stbl", bytes.Join([][]byte{stsd, stts, stsz, stsc, stco}, nil))
	mdhdBody := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdBody[8:12], 48_000)
	binary.BigEndian.PutUint32(mdhdBody[12:16], 960)
	mdia := box("mdia", append(full("mdhd", mdhdBody), box("minf", stbl)...))
	tkhdBody := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdBody[8:12], 1)
	trak := box("trak", append(full("tkhd", tkhdBody), mdia...))
	mvhdBody := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhdBody[8:12], 1000)
	binary.BigEndian.PutUint32(mvhdBody[12:16], 20)
	moov := box("moov", append(full("mvhd", mvhdBody), trak...))
	return append(append(ftyp, moov...), box("mdat", []byte{0xf8, 0x01})...)
}
