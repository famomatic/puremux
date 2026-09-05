package media

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/famomatic/puremux/pkg/bitstream/aac"
)

func TestOpenHLSMasterAES128DiscontinuityAndSeek(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	plain7, _ := aac.WrapADTS(config, []byte{7, 7})
	plain8, _ := aac.WrapADTS(config, []byte{8, 8, 8})
	key := []byte("0123456789abcdef")
	cipher7 := encryptHLSTestSegment(key, 7, plain7)
	cipher8 := encryptHLSTestSegment(key, 8, plain8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "hls" {
			http.Error(w, "missing caller header", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=999999,AUDIO=\"aud\"\nvideo.m3u8\n")
		case "/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:7\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:1,\nseg7.aac\n#EXT-X-DISCONTINUITY\n#EXTINF:1,\nseg8.aac\n#EXT-X-ENDLIST\n")
		case "/key.bin":
			w.Write(key)
		case "/seg7.aac":
			w.Write(cipher7)
		case "/seg8.aac":
			w.Write(cipher8)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	d, err := OpenHLS(context.Background(), server.URL+"/master.m3u8", HLSOptions{Client: server.Client(), Header: http.Header{"X-Test": {"hls"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	streams := d.Streams()
	if len(streams) != 1 || streams[0].Codec != CodecAAC || streams[0].SampleRate != 44100 {
		t.Fatalf("unexpected streams: %+v", streams)
	}
	first, err := d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(first.Data, []byte{7, 7}) || first.PTS.Value != 0 || first.Flags&PacketDiscontinuity != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(second.Data, []byte{8, 8, 8}) || second.PTS.Value != 44100 || second.Flags&PacketDiscontinuity == 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	result, err := d.Seek(context.Background(), SeekRequest{StreamIndex: -1, Target: int64(time.Second)})
	if err != nil || result.Timestamp != int64(time.Second) {
		t.Fatalf("seek=%+v err=%v", result, err)
	}
	second, err = d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(second.Data, []byte{8, 8, 8}) || second.PTS.Value != 44100 {
		t.Fatalf("packet after seek=%+v err=%v", second, err)
	}
}

func TestHLSLivePlaylistRefresh(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48000, FrequencyIndex: 3, ChannelConfig: 2}
	seg0, _ := aac.WrapADTS(config, []byte{0})
	seg1, _ := aac.WrapADTS(config, []byte{1})
	var manifestRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			request := manifestRequests.Add(1)
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1,\n0.aac\n")
			if request > 1 {
				fmt.Fprint(w, "#EXTINF:1,\n1.aac\n#EXT-X-ENDLIST\n")
			}
		case "/0.aac":
			w.Write(seg0)
		case "/1.aac":
			w.Write(seg1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	d, err := OpenHLS(context.Background(), server.URL+"/live.m3u8", HLSOptions{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.ReadPacket(context.Background())
	if err != nil || first.Data[0] != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := d.ReadPacket(context.Background())
	if err != nil || second.Data[0] != 1 || second.PTS.Value != 48000 || manifestRequests.Load() != 2 {
		t.Fatalf("second=%+v requests=%d err=%v", second, manifestRequests.Load(), err)
	}
}

func TestHLSPreservesCrossTrackTimestampOffset(t *testing.T) {
	var segment bytes.Buffer
	muxer, err := NewMuxer(&segment, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	video, err := muxer.AddStream(Stream{Type: MediaVideo, Codec: CodecVP9,
		TimeBase: nanosecondTimeBase, Width: 16, Height: 16})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := muxer.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus,
		TimeBase: nanosecondTimeBase, Channels: 2, SampleRate: 48000,
		Config: CodecConfig{Format: CodecConfigOpusHead,
			Data: append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	// VP9's uncompressed header is LSB-first: 0x82 is a shown profile-0
	// key frame. The Opus payload is one RFC 6716 TOC-only 20 ms packet.
	if err := muxer.WritePacket(context.Background(), &Packet{StreamIndex: video,
		PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(int64(20 * time.Millisecond)),
		Flags: PacketKeyframe, Data: []byte{0x82}}); err != nil {
		t.Fatal(err)
	}
	if err := muxer.WritePacket(context.Background(), &Packet{StreamIndex: audio,
		PTS: KnownTimestamp(int64(20 * time.Millisecond)), DTS: KnownTimestamp(int64(20 * time.Millisecond)),
		Duration: KnownTimestamp(int64(20 * time.Millisecond)), Flags: PacketKeyframe, Data: []byte{0xf8}}); err != nil {
		t.Fatal(err)
	}
	if err := muxer.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nsegment.webm\n#EXT-X-ENDLIST\n")
		case "/segment.webm":
			_, _ = w.Write(segment.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	demuxer, err := OpenHLS(context.Background(), server.URL+"/index.m3u8", HLSOptions{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	first, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.StreamIndex != 0 || first.PTS.Value != 0 || second.StreamIndex != 1 || second.PTS.Value != int64(20*time.Millisecond) {
		t.Fatalf("shifted packets = stream/PTS %d/%d then %d/%d", first.StreamIndex, first.PTS.Value, second.StreamIndex, second.PTS.Value)
	}
}

func TestHLSIntraSegmentSeekKeepsLocalTimestamp(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 44100, FrequencyIndex: 4, ChannelConfig: 2}
	first, _ := aac.WrapADTS(config, []byte{1})
	second, _ := aac.WrapADTS(config, []byte{2})
	segment := append(first, second...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nsegment.aac\n#EXT-X-ENDLIST\n")
		case "/segment.aac":
			_, _ = w.Write(segment)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	demuxer, err := OpenHLS(context.Background(), server.URL+"/index.m3u8", HLSOptions{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer demuxer.Close()
	result, err := demuxer.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 1024})
	if err != nil || result.Timestamp != 1024 {
		t.Fatalf("seek result = %+v, %v", result, err)
	}
	p, err := demuxer.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if p.PTS.Value != 1024 || !bytes.Equal(p.Data, []byte{2}) {
		t.Fatalf("packet after intra-segment seek = %+v", p)
	}
}

func TestHLSLimitsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\na.aac\n")
	}))
	defer server.Close()
	if _, err := OpenHLS(context.Background(), server.URL, HLSOptions{Client: server.Client(), MaxManifestBytes: 4}); err == nil {
		t.Fatal("manifest size limit not enforced")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenHLS(canceled, server.URL, HLSOptions{Client: server.Client()}); err == nil {
		t.Fatal("canceled open succeeded")
	}
}

func TestHLSContentRangeValidation(t *testing.T) {
	if !validHLSContentRange("bytes 10-19/100", 10, 19) {
		t.Fatal("valid Content-Range rejected")
	}
	for _, value := range []string{"", "bytes 9-19/100", "bytes 10-20/100", "bytes 10-19/19", "items 10-19/100", "bytes x-y/100"} {
		if validHLSContentRange(value, 10, 19) {
			t.Fatalf("invalid Content-Range accepted: %q", value)
		}
	}
}

func encryptHLSTestSegment(key []byte, sequence uint64, plain []byte) []byte {
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	var iv [16]byte
	binary.BigEndian.PutUint64(iv[8:], sequence)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(padded, padded)
	return padded
}
