package mpegts

import (
	"bytes"
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
	value, err := decodeTimestamp([]byte{0x21, 0x00, 0x37, 0x77, 0x41})
	if err != nil || value != 900000 {
		t.Fatalf("timestamp=%d err=%v", value, err)
	}
	for _, data := range [][]byte{nil, {0x21, 0, 0x36, 0x77, 0x41}, {0x20, 0, 0x37, 0x77, 0x41}} {
		if _, err := decodeTimestamp(data); err == nil {
			t.Fatalf("accepted malformed timestamp % X", data)
		}
	}
	if _, err := NewInputReader(bytes.NewReader([]byte{0x47})); err == nil {
		t.Fatal("accepted truncated transport packet")
	}
}
