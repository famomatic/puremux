package media

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/famomatic/puremux/pkg/bitstream/aac"
)

func TestOpenDASHSegmentListRangesAndSeek(t *testing.T) {
	config := aac.Config{AudioObjectType: 2, SampleRate: 48000, FrequencyIndex: 3, ChannelConfig: 2}
	first, _ := aac.WrapADTS(config, []byte{1, 2})
	second, _ := aac.WrapADTS(config, []byte{3, 4, 5})
	all := append(append([]byte(nil), first...), second...)
	var rangeRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DASH-Test") != "yes" {
			http.Error(w, "header", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/manifest.mpd":
			fmt.Fprintf(w, `<MPD type="static" mediaPresentationDuration="PT2S"><Period duration="PT2S"><AdaptationSet contentType="video"><Representation id="v" bandwidth="999"><BaseURL>video.bin</BaseURL></Representation></AdaptationSet><AdaptationSet contentType="audio" mimeType="audio/aac"><Representation id="a" bandwidth="128"><BaseURL>all.aac</BaseURL><SegmentList timescale="1" duration="1"><SegmentURL mediaRange="0-%d"/><SegmentURL mediaRange="%d-%d"/></SegmentList></Representation></AdaptationSet></Period></MPD>`, len(first)-1, len(first), len(all)-1)
		case "/all.aac":
			rangeRequests.Add(1)
			value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
			parts := strings.Split(value, "-")
			start, _ := strconv.Atoi(parts[0])
			end, _ := strconv.Atoi(parts[1])
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(all)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(all[start : end+1])
		case "/video.bin":
			http.Error(w, "video must not be selected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	d, err := OpenDASH(context.Background(), server.URL+"/manifest.mpd", DASHOptions{Client: server.Client(), Header: http.Header{"X-DASH-Test": {"yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if info := d.Info(); info.Format != FormatDASH || !info.DurationKnown || info.Duration != 2*time.Second {
		t.Fatalf("unexpected info: %+v", info)
	}
	if streams := d.Streams(); len(streams) != 1 || streams[0].Codec != CodecAAC || streams[0].SampleRate != 48000 {
		t.Fatalf("unexpected streams: %+v", streams)
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(p.Data, []byte{1, 2}) || p.PTS.Value != 0 {
		t.Fatalf("first=%+v err=%v", p, err)
	}
	p, err = d.ReadPacket(context.Background())
	if err != nil || !bytes.Equal(p.Data, []byte{3, 4, 5}) || p.PTS.Value != 48000 {
		t.Fatalf("second=%+v err=%v", p, err)
	}
	result, err := d.Seek(context.Background(), SeekRequest{StreamIndex: -1, Target: int64(time.Second)})
	if err != nil || result.Timestamp != int64(time.Second) {
		t.Fatalf("seek=%+v err=%v", result, err)
	}
	p, err = d.ReadPacket(context.Background())
	if err != nil || p.Data[0] != 3 || rangeRequests.Load() != 3 {
		t.Fatalf("after seek=%+v requests=%d err=%v", p, rangeRequests.Load(), err)
	}
}

func TestOpenDASHLimitsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<MPD><Period><AdaptationSet><Representation id="a"><BaseURL>a.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`)
	}))
	defer server.Close()
	if _, err := OpenDASH(context.Background(), server.URL, DASHOptions{Client: server.Client(), MaxManifestBytes: 8}); err == nil {
		t.Fatal("manifest bound not enforced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenDASH(ctx, server.URL, DASHOptions{Client: server.Client()}); err == nil {
		t.Fatal("canceled open succeeded")
	}
}
