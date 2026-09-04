package mpegts

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
)

func TestInputReaderADTSRoundTrip(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48000, FrequencyIndex: 3, ChannelConfig: 2}
	frame, _ := aac.WrapADTS(config, []byte{0x11, 0x22, 0x33})
	var output bytes.Buffer
	mux := New(&output)
	track, err := mux.AddTrack(core.Track{ID: 7, Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(&core.Packet{TrackID: track, Codec: core.CodecAAC, Data: frame, PTS: time.Second, DTS: time.Second}); err != nil {
		t.Fatal(err)
	}
	r, err := NewInputReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tracks := r.Tracks()
	if len(tracks) != 1 || tracks[0].Codec != core.CodecAAC || tracks[0].SampleRate != 48000 || tracks[0].Channels != 2 || !bytes.Equal(tracks[0].Config, []byte{0x11, 0x90}) {
		t.Fatalf("unexpected track: %+v", tracks)
	}
	p, err := r.NextPacket()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, []byte{0x11, 0x22, 0x33}) || p.PTS != 480000 || p.Duration != 1024 {
		t.Fatalf("unexpected packet: %+v", p)
	}
}

func TestDecodeTimestampSpecBytesAndBoundaries(t *testing.T) {
	// ISO 13818-1 PTS=900000, packed MSB-first with marker bits.
	value, err := decodeTimestamp([]byte{0x21, 0x00, 0x37, 0x77, 0x41}, 2)
	if err != nil || value != 900000 {
		t.Fatalf("timestamp=%d err=%v", value, err)
	}
	for _, data := range [][]byte{nil, {0x21, 0, 0x36, 0x77, 0x41}, {0x20, 0, 0x37, 0x77, 0x41}} {
		if _, err := decodeTimestamp(data, 2); err == nil {
			t.Fatalf("accepted malformed timestamp % X", data)
		}
	}
	if _, err := NewInputReader(bytes.NewReader([]byte{0x47})); err == nil {
		t.Fatal("accepted truncated transport packet")
	}
}

func TestParsePESUsesDeclaredLengthAndExactAudioClock(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	first, err := aac.WrapADTS(config, []byte{0x11})
	if err != nil {
		t.Fatal(err)
	}
	second, err := aac.WrapADTS(config, []byte{0x22})
	if err != nil {
		t.Fatal(err)
	}
	payload := append(first, second...)
	// ISO/IEC 13818-1 PES: stream_id C0, PTS-only flags, five-byte PTS
	// 900000 encoded as 21 00 37 77 41. PES_packet_length counts bytes
	// following its own field: flags(3) + PTS(5) + payload.
	data := []byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0x80, 5, 0x21, 0x00, 0x37, 0x77, 0x41}
	binary.BigEndian.PutUint16(data[4:6], uint16(8+len(payload)))
	data = append(data, payload...)
	data = append(data, 0xff, 0xff, 0xff) // transport padding outside PES_packet_length
	packets, track, err := parsePES(&pesBuffer{pid: 256, data: data}, 0, core.CodecAAC)
	if err != nil {
		t.Fatal(err)
	}
	if track.SampleRate != 44100 || len(packets) != 2 {
		t.Fatalf("track=%+v packets=%d", track, len(packets))
	}
	if packets[0].PTS != 441000 || packets[1].PTS != 442024 || packets[1].Duration != 1024 {
		t.Fatalf("packet timestamps = %d, %d duration %d", packets[0].PTS, packets[1].PTS, packets[1].Duration)
	}
	if !bytes.Equal(packets[1].Data, []byte{0x22}) {
		t.Fatalf("second payload includes transport padding: % X", packets[1].Data)
	}
}

func TestParsePESRejectsFlagsPrefixesAndTruncation(t *testing.T) {
	tests := [][]byte{
		{0, 0, 1, 0xe0, 0, 3, 0x80, 0x40, 0},                                       // forbidden flags 01
		{0, 0, 1, 0xe0, 0, 8, 0x80, 0x80, 5, 0x31, 0, 1, 0, 1},                     // PTS-only requires 0010 prefix
		{0, 0, 1, 0xe0, 0, 13, 0x80, 0xc0, 10, 0x21, 0, 1, 0, 1, 0x11, 0, 1, 0, 1}, // pair PTS requires 0011
		{0, 0, 1, 0xe0, 0, 20, 0x80, 0, 0},                                         // declared packet overruns buffer
	}
	for _, data := range tests {
		if _, _, err := parsePES(&pesBuffer{data: data}, 0, core.CodecH264); err == nil {
			t.Fatalf("accepted malformed PES: % X", data)
		}
	}
}

func TestInputReaderRejectsElementaryConfigurationChange(t *testing.T) {
	firstConfig := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	secondConfig := aac.Config{AudioObjectType: 2, SampleRate: 48000, FrequencyIndex: 3, ChannelConfig: 2}
	first, _ := aac.WrapADTS(firstConfig, []byte{1})
	second, _ := aac.WrapADTS(secondConfig, []byte{2})
	var output bytes.Buffer
	mux := New(&output)
	track, err := mux.AddTrack(core.Track{ID: 1, Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(&core.Packet{TrackID: track, Codec: core.CodecAAC, Data: first}); err != nil {
		t.Fatal(err)
	}
	if err := mux.WritePacket(&core.Packet{TrackID: track, Codec: core.CodecAAC, Data: second, PTS: time.Second, DTS: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInputReader(bytes.NewReader(output.Bytes())); err == nil {
		t.Fatal("AAC elementary configuration change was accepted")
	}
}
