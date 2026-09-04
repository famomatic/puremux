package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

type captureMuxer struct {
	streams   []Stream
	packets   []*Packet
	closed    int
	err       error
	addErr    error
	indexBase int
}

func (m *captureMuxer) AddStream(stream Stream) (int, error) {
	if m.addErr != nil {
		return 0, m.addErr
	}
	stream.Index = m.indexBase + len(m.streams)
	m.streams = append(m.streams, stream)
	return stream.Index, nil
}

func (m *captureMuxer) WritePacket(_ context.Context, packet *Packet) error {
	if m.err != nil {
		return m.err
	}
	copyPacket := &Packet{
		StreamIndex:    packet.StreamIndex,
		Data:           append([]byte(nil), packet.Data...),
		PTS:            packet.PTS,
		DTS:            packet.DTS,
		Duration:       packet.Duration,
		Flags:          packet.Flags,
		Pos:            packet.Pos,
		DiscardPadding: packet.DiscardPadding,
	}
	m.packets = append(m.packets, copyPacket)
	return nil
}

func (m *captureMuxer) Close() error { m.closed++; return m.err }

type liveBitWriter struct {
	buf []byte
	bit uint
}

func (w *liveBitWriter) u(value uint32, count uint) {
	for i := count; i > 0; i-- {
		if w.bit%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if value>>(i-1)&1 != 0 {
			w.buf[len(w.buf)-1] |= 0x80 >> (w.bit % 8)
		}
		w.bit++
	}
}

func (w *liveBitWriter) ue(value uint32) {
	code := value + 1
	bits := uint(0)
	for n := code; n > 0; n >>= 1 {
		bits++
	}
	w.u(0, bits-1)
	w.u(code, bits)
}

// liveSPS/livePPS and liveH264AU follow ITU-T H.264 7.3.2.1, 7.3.2.2,
// and 7.3.3, MSB-first. The SPS is Baseline profile with pic_order_cnt_type 0,
// a 6-bit pic_order_cnt_lsb, and a 4-bit frame_num. The final byte tags decode
// order without affecting the header fields consumed by the parser.
var (
	liveSPS = []byte{0x67, 0x42, 0x00, 0x1e, 0xec, 0xa0, 0xa0, 0xfc}
	livePPS = []byte{0x68, 0xc8}
)

func liveH264AU(idr, reference bool, frameNum, pocLSB uint32, tag byte, params bool) []byte {
	w := &liveBitWriter{}
	w.ue(0)
	if idr {
		w.ue(7)
	} else {
		w.ue(0)
	}
	w.ue(0)
	w.u(frameNum, 4)
	if idr {
		w.ue(0)
	}
	w.u(pocLSB, 6)
	w.u(1, 1)
	header := byte(0x01)
	if idr {
		header = 0x05
	}
	if reference {
		header |= 0x60
	}
	nal := append([]byte{header}, w.buf...)
	nal = append(nal, tag)
	var au []byte
	if params {
		au = append(au, 0, 0, 0, 1)
		au = append(au, liveSPS...)
		au = append(au, 0, 0, 0, 1)
		au = append(au, livePPS...)
	}
	au = append(au, 0, 0, 0, 1)
	return append(au, nal...)
}

func liveBFrameGOP(groups int) ([][]byte, []int) {
	type picture struct {
		order int64
		index int
	}
	aus := [][]byte{liveH264AU(true, true, 0, 0, 0, true)}
	pictures := []picture{{order: 0, index: 0}}
	base, frameNum, index := int64(0), uint32(1), 1
	for range groups {
		for _, entry := range []struct {
			offset    int64
			reference bool
		}{{6, true}, {2, true}, {4, false}} {
			order := base + entry.offset
			aus = append(aus, liveH264AU(false, entry.reference, frameNum%16, uint32(order%64), byte(index), false))
			pictures = append(pictures, picture{order: order, index: index})
			if entry.reference {
				frameNum++
			}
			index++
		}
		base += 6
	}
	ordered := slices.Clone(pictures)
	slices.SortFunc(ordered, func(a, b picture) int { return int(a.order - b.order) })
	ranks := make([]int, len(pictures))
	for rank, picture := range ordered {
		ranks[picture.index] = rank
	}
	return aus, ranks
}

func TestLiveMuxerSynthesizesH264PresentationOrder(t *testing.T) {
	sink := &captureMuxer{}
	opts := DefaultLiveIngestOptions()
	opts.MinMonotonicStep = time.Millisecond
	live, err := NewLiveMuxer(sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	video, err := live.AddStream(Stream{Type: MediaVideo, Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 1000}, DefaultPacket: 17 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	aus, ranks := liveBFrameGOP(8)
	for i, au := range aus {
		original := append([]byte(nil), au...)
		if err := live.WriteVideo(context.Background(), video, au, int64(i*17)); err != nil {
			t.Fatalf("WriteVideo %d: %v", i, err)
		}
		clear(au)
		aus[i] = original
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.packets) != len(aus) {
		t.Fatalf("packets=%d want %d", len(sink.packets), len(aus))
	}
	sawSplit := false
	byRank := make([]int64, len(sink.packets))
	for i, packet := range sink.packets {
		if !bytes.Equal(packet.Data, aus[i]) {
			t.Fatalf("decode order or caller-buffer ownership changed at %d", i)
		}
		if i > 0 && packet.DTS.Value <= sink.packets[i-1].DTS.Value {
			t.Fatalf("DTS not monotonic at %d", i)
		}
		if packet.DTS.Value > packet.PTS.Value || packet.Duration.Value <= 0 {
			t.Fatalf("invalid timing at %d: DTS=%d PTS=%d duration=%d", i, packet.DTS.Value, packet.PTS.Value, packet.Duration.Value)
		}
		if packet.DTS.Value != packet.PTS.Value {
			sawSplit = true
		}
		byRank[ranks[i]] = packet.PTS.Value
	}
	if !sawSplit {
		t.Fatal("POC presentation synthesis did not produce distinct PTS/DTS")
	}
	for i := 1; i < len(byRank); i++ {
		if byRank[i] <= byRank[i-1] {
			t.Fatalf("display-order PTS not monotonic at rank %d", i)
		}
	}
}

func TestLiveMuxerGenericPacketPath(t *testing.T) {
	sink := &captureMuxer{}
	live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	video, err := live.AddStream(Stream{Type: MediaVideo, Codec: CodecVP9, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := live.AddStream(Stream{Type: MediaData, Codec: CodecUnknown, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}

	write := func(packet *Packet) {
		t.Helper()
		if err := live.WritePacket(context.Background(), packet); err != nil {
			t.Fatal(err)
		}
		clear(packet.Data)
	}
	write(&Packet{StreamIndex: audio, Data: []byte{0x10}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(10)})
	write(&Packet{StreamIndex: data, Data: []byte{0x20}, PTS: KnownTimestamp(10), DTS: KnownTimestamp(10), Duration: KnownTimestamp(5),
		Flags: PacketCorrupt | PacketDiscontinuity, Pos: 42, DiscardPadding: -5 * time.Millisecond})
	write(&Packet{StreamIndex: video, Data: []byte{0x30}, PTS: KnownTimestamp(20), DTS: KnownTimestamp(20),
		Duration: KnownTimestamp(10), Flags: PacketKeyframe})
	write(&Packet{StreamIndex: audio, Data: []byte{0x40}, PTS: KnownTimestamp(30), DTS: KnownTimestamp(30)})
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.packets) != 3 {
		t.Fatalf("packets=%d want 3; pre-keyframe audio should be dropped", len(sink.packets))
	}
	if got := []byte{sink.packets[0].Data[0], sink.packets[1].Data[0], sink.packets[2].Data[0]}; !slices.Equal(got, []byte{0x20, 0x30, 0x40}) {
		t.Fatalf("generic packet order/data=%x", got)
	}
	generic := sink.packets[0]
	if generic.StreamIndex != data || generic.Flags != PacketCorrupt|PacketDiscontinuity || generic.Pos != 42 ||
		generic.DiscardPadding != -5*time.Millisecond || generic.Duration.Value != 5 {
		t.Fatalf("generic metadata not preserved: %+v", generic)
	}
	if sink.packets[1].StreamIndex != video || !sink.packets[1].Keyframe() {
		t.Fatalf("video keyframe metadata not preserved: %+v", sink.packets[1])
	}
	if sink.packets[2].StreamIndex != audio || sink.packets[2].Duration.Value <= 0 {
		t.Fatalf("audio lookahead duration not completed: %+v", sink.packets[2])
	}
}

func TestLiveMuxerPreservesSubTwoNanosecondTicks(t *testing.T) {
	sink := &captureMuxer{}
	live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	index, err := live.AddStream(Stream{Type: MediaData, Codec: CodecUnknown, TimeBase: Rational{Num: 1, Den: 600_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.WritePacket(context.Background(), &Packet{
		StreamIndex: index, Data: []byte{1}, PTS: KnownTimestamp(1), DTS: KnownTimestamp(1), Duration: KnownTimestamp(1),
	}); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.packets) != 1 || sink.packets[0].PTS.Value != 1 || sink.packets[0].DTS.Value != 1 || sink.packets[0].Duration.Value != 1 {
		t.Fatalf("sub-2ns tick did not round-trip: %+v", sink.packets)
	}
}

func TestLiveMuxerDiscontinuityDoesNotBecomePacketDuration(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags PacketFlags
	}{
		{name: "threshold"},
		{name: "flag", flags: PacketDiscontinuity},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &captureMuxer{}
			live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
			if err != nil {
				t.Fatal(err)
			}
			index, err := live.AddStream(Stream{
				Type: MediaData, Codec: CodecUnknown, TimeBase: Rational{Num: 1, Den: 1000}, DefaultPacket: 20 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, packet := range []*Packet{
				{StreamIndex: index, Data: []byte{1}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0)},
				{StreamIndex: index, Data: []byte{2}, PTS: KnownTimestamp(10_000), DTS: KnownTimestamp(10_000), Flags: test.flags},
			} {
				if err := live.WritePacket(context.Background(), packet); err != nil {
					t.Fatal(err)
				}
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			if len(sink.packets) != 2 || sink.packets[0].Duration.Value != 20 {
				t.Fatalf("pre-discontinuity duration=%+v want 20 ticks", sink.packets)
			}
		})
	}
}

func TestLiveMuxerAppliesConfiguredBoundToAVWait(t *testing.T) {
	sink := &captureMuxer{}
	opts := DefaultLiveIngestOptions()
	opts.MaxBufferPackets = 2
	opts.JitterWindow = time.Nanosecond
	live, err := NewLiveMuxer(sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	video, err := live.AddStream(Stream{Type: MediaVideo, Codec: CodecVP9, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := live.WritePacket(context.Background(), &Packet{
			StreamIndex: audio, Data: []byte{byte(i)}, PTS: KnownTimestamp(int64(i * 10)), DTS: KnownTimestamp(int64(i * 10)), Duration: KnownTimestamp(10),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := live.WritePacket(context.Background(), &Packet{
		StreamIndex: video, Data: []byte{0x80}, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(10), Flags: PacketKeyframe,
	}); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	var audioData []byte
	for _, packet := range sink.packets {
		if packet.StreamIndex == audio {
			audioData = append(audioData, packet.Data[0])
		}
	}
	if !slices.Equal(audioData, []byte{1, 2, 3}) {
		t.Fatalf("bounded A/V wait retained audio=%v want [1 2 3]", audioData)
	}
	metrics, ok := live.Metrics(audio)
	if !ok || metrics.AudioPacketsDropped != 1 {
		t.Fatalf("audio metrics=%+v ok=%v", metrics, ok)
	}
}

func TestLiveMuxerInterleaveBound(t *testing.T) {
	sink := &captureMuxer{}
	opts := DefaultLiveIngestOptions()
	opts.JitterWindow = time.Nanosecond
	opts.MaxInterleavePackets = 2
	live, err := NewLiveMuxer(sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	active, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 48_000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 48_000}}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		err = live.WritePacket(context.Background(), &Packet{
			StreamIndex: active, Data: []byte{byte(i)},
			PTS: KnownTimestamp(int64(i * 1024)), DTS: KnownTimestamp(int64(i * 1024)), Duration: KnownTimestamp(1024),
		})
	}
	if !errors.Is(err, ErrLiveBufferFull) {
		t.Fatalf("interleave error=%v", err)
	}
	if err := live.Close(); !errors.Is(err, ErrLiveBufferFull) || sink.closed != 1 {
		t.Fatalf("close error=%v count=%d", err, sink.closed)
	}
}

func TestLiveMuxerReordersJitterAndNudgesDuplicateDTS(t *testing.T) {
	sink := &captureMuxer{indexBase: 7}
	opts := DefaultLiveIngestOptions()
	live, err := NewLiveMuxer(sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	video, err := live.AddStream(Stream{
		Type: MediaVideo, Codec: CodecH264,
		TimeBase: Rational{Num: 1, Den: 1000}, DefaultPacket: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if video != 7 {
		t.Fatalf("stream index=%d want 7", video)
	}

	// Eight parseable pictures finish the no-B-frame probe. Duplicated and
	// scrambled stamps model a live startup backfill burst; the 400 ms
	// enforcer must sort by the supplied decode clock, preserve FIFO among
	// duplicates, and nudge collisions by at least 1 ms.
	decodeClocks := []int64{0, 0, 2, 1, 4, 3, 6, 5}
	for i := range 8 {
		au := liveH264AU(i == 0, true, uint32(i%16), uint32(i*2), byte(i), i == 0)
		if err := live.WriteVideo(context.Background(), video, au, decodeClocks[i]); err != nil {
			t.Fatalf("WriteVideo %d: %v", i, err)
		}
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.packets) != 8 {
		t.Fatalf("packets=%d want 8", len(sink.packets))
	}
	wantTags := []byte{0, 1, 3, 2, 5, 4, 7, 6}
	for i, packet := range sink.packets {
		if packet.StreamIndex != video || packet.Data[len(packet.Data)-1] != wantTags[i] {
			t.Fatalf("jitter order[%d]=%d want %d", i, packet.Data[len(packet.Data)-1], wantTags[i])
		}
		if packet.DTS.Value != int64(i) {
			t.Fatalf("DTS[%d]=%d want %d", i, packet.DTS.Value, i)
		}
	}
	if _, ok := live.Metrics(video); !ok {
		t.Fatal("metrics lookup by muxer-assigned stream index failed")
	}
}

// adts48kStereo packs an MPEG-4 AAC-LC, 48 kHz, two-channel ADTS frame
// MSB-first per ISO/IEC 14496-3 1.A.2.2. frame_length includes the seven-byte
// header: FF F1 4C 80 01 5F FC encodes a ten-byte frame with one raw block.
func adts48kStereo(tag byte) []byte {
	return []byte{0xff, 0xf1, 0x4c, 0x80, 0x01, 0x5f, 0xfc, tag, 0xaa, 0xbb}
}

func TestLiveMuxerAlignsIDRAndSplitsADTS(t *testing.T) {
	sink := &captureMuxer{}
	live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	video, err := live.AddStream(Stream{Type: MediaVideo, Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 1_000_000_000}, DefaultPacket: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 1_000_000_000}, Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.WriteVideo(context.Background(), video, liveH264AU(false, true, 1, 2, 0x10, false), 0); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteADTS(context.Background(), audio, adts48kStereo(0x20), 0); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteVideo(context.Background(), video, liveH264AU(true, true, 0, 0, 0x30, true), int64(20*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	chunk := append(adts48kStereo(0x40), adts48kStereo(0x50)...)
	if err := live.WriteADTS(context.Background(), audio, chunk, int64(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteVideo(context.Background(), video, liveH264AU(false, true, 1, 2, 0x60, false), int64(40*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	var audioTags []byte
	for _, packet := range sink.packets {
		if packet.StreamIndex == audio {
			audioTags = append(audioTags, packet.Data[7])
			if packet.Duration.Value != int64(1024*time.Second/48000) {
				t.Fatalf("AAC duration=%d", packet.Duration.Value)
			}
		}
		if packet.StreamIndex == video && !packet.Keyframe() && packet.Data[len(packet.Data)-1] == 0x10 {
			t.Fatal("pre-IDR video packet was not dropped")
		}
	}
	if !slices.Equal(audioTags, []byte{0x50}) {
		t.Fatalf("audio tags=%x want 50; pre-IDR frames must be dropped", audioTags)
	}
}

func TestLiveMuxerADTSPreservesSampleClockTicks(t *testing.T) {
	sink := &captureMuxer{}
	live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	audio, err := live.AddStream(Stream{
		Type: MediaAudio, Codec: CodecAAC,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// At 48 kHz, converting each 1024-sample duration independently to
	// nanoseconds loses 1/3 ns per frame. This count exceeds half a sample
	// tick of cumulative loss, so an incremental-duration implementation
	// would finish one tick early. Computing each clock from the exact total
	// sample count must remain drift-free.
	const frameCount = 32_770
	chunk := make([]byte, 0, frameCount*10)
	for i := range frameCount {
		chunk = append(chunk, adts48kStereo(byte(i))...)
	}
	if err := live.WriteADTS(context.Background(), audio, chunk, 0); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.packets) != frameCount {
		t.Fatalf("packets=%d want %d", len(sink.packets), frameCount)
	}
	last := sink.packets[len(sink.packets)-1]
	wantLast := int64(frameCount-1) * 1024
	if last.PTS.Value != wantLast || last.DTS.Value != wantLast || last.Duration.Value != 1024 {
		t.Fatalf("last packet timing: PTS=%d DTS=%d duration=%d want clock=%d duration=1024", last.PTS.Value, last.DTS.Value, last.Duration.Value, wantLast)
	}
}

func TestLiveMuxerMPEGTSRoundTrip(t *testing.T) {
	var output bytes.Buffer
	muxer, err := NewMuxer(&output, MuxOptions{Format: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	live, err := NewLiveMuxer(muxer, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	audio, err := live.AddStream(Stream{
		Type: MediaAudio, Codec: CodecAAC,
		TimeBase: Rational{Num: 1, Den: 48_000}, SampleRate: 48_000, Channels: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := append(adts48kStereo(0x61), adts48kStereo(0x62)...)
	if err := live.WriteADTS(context.Background(), audio, chunk, 0); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Len()%188 != 0 || output.Bytes()[0] != 0x47 {
		t.Fatalf("invalid TS framing: size=%d", output.Len())
	}

	demuxer, err := Open(context.Background(), MemorySource("live.ts", output.Bytes()), OpenOptions{FormatHint: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	var tags []byte
	for {
		packet, err := demuxer.ReadPacket(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(packet.Data) != 3 {
			t.Fatalf("AAC payload=%x", packet.Data)
		}
		tags = append(tags, packet.Data[0])
		packet.Release()
	}
	if !slices.Equal(tags, []byte{0x61, 0x62}) {
		t.Fatalf("AAC tags=%x", tags)
	}
}

func TestLiveMuxerBoundariesAndClose(t *testing.T) {
	if _, ok := livePacketDuration(time.Duration(-1<<63), time.Duration(1<<63-1)); ok {
		t.Fatal("overflowing packet duration accepted")
	}
	if got, ok := livePacketDuration(-time.Second, time.Second); !ok || got != 2*time.Second {
		t.Fatalf("packet duration=%v ok=%v", got, ok)
	}
	timeBase := Rational{Num: 1, Den: 48_000}
	frameDuration, ok := timeBase.Duration(1024)
	if !ok {
		t.Fatal("frame duration conversion failed")
	}
	if got, ok := durationToLiveTicks(frameDuration, timeBase); !ok || got != 1024 {
		t.Fatalf("positive nearest tick=%d ok=%v", got, ok)
	}
	if got, ok := durationToLiveTicks(-frameDuration, timeBase); !ok || got != -1024 {
		t.Fatalf("negative nearest tick=%d ok=%v", got, ok)
	}
	step, ok := liveTimeBaseStep(timeBase)
	if !ok {
		t.Fatal("48 kHz monotonic step conversion failed")
	}
	previous := int64(-1)
	for i := int64(0); i < 40_000; i++ {
		got, ok := durationToLiveTicks(time.Duration(i)*step, timeBase)
		if !ok || got <= previous {
			t.Fatalf("48 kHz repaired tick collapsed at %d: got=%d previous=%d", i, got, previous)
		}
		previous = got
	}
	if _, ok := durationToLiveTicks(time.Duration(1<<63-1), Rational{}); ok {
		t.Fatal("invalid time base converted extreme duration")
	}
	if _, ok := durationToLiveTicks(time.Duration(1<<63-1), timeBase); !ok {
		t.Fatal("MaxInt64 duration overflowed nearest-tick conversion")
	}
	if _, ok := durationToLiveTicks(time.Duration(-1<<63), timeBase); !ok {
		t.Fatal("MinInt64 duration overflowed nearest-tick conversion")
	}

	if _, err := NewLiveMuxer(nil, LiveIngestOptions{}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("nil muxer err=%v", err)
	}
	if _, err := NewLiveMuxer(&captureMuxer{}, LiveIngestOptions{JitterWindow: -1}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("negative option err=%v", err)
	}
	for _, timeBase := range []Rational{
		{Num: 1, Den: 2_000_000_000},
		{Num: 1<<63 - 1, Den: 1},
	} {
		invalidLive, err := NewLiveMuxer(&captureMuxer{}, DefaultLiveIngestOptions())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := invalidLive.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: timeBase}); !errors.Is(err, ErrInvalidData) {
			t.Fatalf("unrepresentable time base %+v err=%v", timeBase, err)
		}
	}
	addFailure := errors.New("add failure")
	addLive, err := NewLiveMuxer(&captureMuxer{addErr: addFailure}, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addLive.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 1000}}); !errors.Is(err, addFailure) {
		t.Fatalf("AddStream error=%v", err)
	}
	sink := &captureMuxer{}
	live, err := NewLiveMuxer(sink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 1_000_000_000}, SampleRate: 44_100, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteADTS(context.Background(), 0, nil, 0); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("nil ADTS err=%v", err)
	}
	if err := live.WriteADTS(context.Background(), 0, []byte{0xff, 0xf1, 0x4c}, 0); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("truncated ADTS err=%v", err)
	}
	if err := live.WriteADTS(context.Background(), 0, adts48kStereo(1), 0); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("sample-rate mismatch err=%v", err)
	}
	if _, err := live.AddStream(Stream{Type: MediaVideo, Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 1000}}); err != nil {
		t.Fatalf("invalid first writes locked stream registration: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil || sink.closed != 1 {
		t.Fatalf("idempotent Close err=%v count=%d", err, sink.closed)
	}
	if err := live.WriteADTS(context.Background(), 0, adts48kStereo(1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close err=%v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	live2, err := NewLiveMuxer(&captureMuxer{}, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	index, err := live2.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 48_000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := live2.WriteADTS(canceled, index, adts48kStereo(2), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write err=%v", err)
	}
	changedRate := adts48kStereo(3)
	changedRate[2] = 0x50 // AAC-LC with sampling_frequency_index 4 (44.1 kHz).
	mixedRates := append(adts48kStereo(2), changedRate...)
	if err := live2.WriteADTS(context.Background(), index, mixedRates, 0); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("mixed ADTS sample rates err=%v", err)
	}
	if err := live2.Close(); err != nil {
		t.Fatal(err)
	}

	sinkFailure := errors.New("sink failure")
	failingSink := &captureMuxer{err: sinkFailure}
	live3, err := NewLiveMuxer(failingSink, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	index, err = live3.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 48_000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := live3.WriteADTS(context.Background(), index, adts48kStereo(3), 0); err != nil {
		t.Fatal(err)
	}
	if err := live3.Close(); !errors.Is(err, sinkFailure) || failingSink.closed != 1 {
		t.Fatalf("sink error=%v close count=%d", err, failingSink.closed)
	}

	mixed, err := NewLiveMuxer(&captureMuxer{}, DefaultLiveIngestOptions())
	if err != nil {
		t.Fatal(err)
	}
	video, err := mixed.AddStream(Stream{Type: MediaVideo, Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mixed.WritePacket(context.Background(), &Packet{StreamIndex: video, Data: []byte{1},
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1), Flags: PacketKeyframe}); err != nil {
		t.Fatal(err)
	}
	if err := mixed.WriteVideo(context.Background(), video, liveH264AU(true, true, 0, 0, 0, true), 1); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("mixed ingestion modes err=%v", err)
	}
	if err := mixed.Close(); err != nil {
		t.Fatal(err)
	}
}
