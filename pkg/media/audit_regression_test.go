package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/famomatic/puremux/internal/manifest"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/opus"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests preserve the contracts established by the 2026-09-05 audit.
// Payloads are opaque;
// ASC 11 90 = AOT 2 (00010), 48kHz index 3 (0011), stereo 2 (0010), GASpecific 000.
func auditAACStream() Stream {
	return Stream{Type: MediaAudio, Codec: CodecAAC, TimeBase: Rational{Num: 1, Den: 48000}, SampleRate: 48000, Channels: 2, Config: CodecConfig{Format: CodecConfigASC, Data: []byte{0x11, 0x90}}}
}
func auditSegment(t *testing.T, start int64) []byte {
	t.Helper()
	var b bytes.Buffer
	m, e := NewMuxer(&b, MuxOptions{Format: FormatMP4, MP4Mode: MP4ModeFragmented})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.AddStream(auditAACStream()); e != nil {
		t.Fatal(e)
	}
	for i := int64(0); i < 3; i++ {
		ts := start + i*1024
		if e = m.WritePacket(context.Background(), &Packet{StreamIndex: 0, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(1024), Flags: PacketKeyframe, Data: []byte{byte(i + 1)}}); e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestAuditManifestRetryAfterNoNewSegments(t *testing.T) {
	for _, kind := range []string{"hls", "dash"} {
		t.Run(kind, func(t *testing.T) {
			frame, e := aac.WrapADTS(aac.Config{AudioObjectType: 2, SampleRate: 48000, FrequencyIndex: 3, ChannelConfig: 2}, []byte{1})
			if e != nil {
				t.Fatal(e)
			}
			var ready atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/index" {
					if kind == "hls" {
						fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:1,\n1.aac\n")
						if ready.Load() {
							fmt.Fprint(w, "#EXTINF:1,\n2.aac\n")
						}
					} else {
						repeat := 0
						if ready.Load() {
							repeat = 1
						}
						fmt.Fprintf(w, `<MPD type="dynamic"><Period><AdaptationSet contentType="audio"><Representation id="a"><SegmentTemplate timescale="1" media="$Number$.aac"><SegmentTimeline><S t="0" d="1" r="%d"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`, repeat)
					}
				} else {
					w.Write(frame)
				}
			}))
			defer srv.Close()
			var d Demuxer
			if kind == "hls" {
				d, e = OpenHLS(context.Background(), srv.URL+"/index", HLSOptions{})
			} else {
				d, e = OpenDASH(context.Background(), srv.URL+"/index", DASHOptions{})
			}
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			p, e := d.ReadPacket(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			p.Release()
			_, e = d.ReadPacket(context.Background())
			if !errors.Is(e, ErrNoNewSegments) {
				t.Fatalf("expected temporary empty playlist, got %v", e)
			}
			ready.Store(true)
			p, e = d.ReadPacket(context.Background())
			if e != nil {
				t.Fatalf("new segment available but retry failed: %v", e)
			}
			p.Release()
		})
	}
}
func TestAuditManifestSeekNonzeroOrigin(t *testing.T) {
	data := auditSegment(t, 480000)
	for _, kind := range []string{"hls", "dash"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/index" {
					if kind == "hls" {
						fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\ns.mp4\n#EXT-X-ENDLIST\n")
					} else {
						fmt.Fprint(w, `<MPD type="static" mediaPresentationDuration="PT1S"><Period><AdaptationSet contentType="audio"><Representation id="a"><SegmentList timescale="1" duration="1"><SegmentURL media="s.mp4"/></SegmentList></Representation></AdaptationSet></Period></MPD>`)
					}
				} else {
					w.Write(data)
				}
			}))
			defer srv.Close()
			var d Demuxer
			var e error
			if kind == "hls" {
				d, e = OpenHLS(context.Background(), srv.URL+"/index", HLSOptions{})
			} else {
				d, e = OpenDASH(context.Background(), srv.URL+"/index", DASHOptions{})
			}
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			result, e := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 1024})
			if e != nil {
				t.Fatal(e)
			}
			p, e := d.ReadPacket(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer p.Release()
			if result.Timestamp != 1024 || p.PTS.Value != 1024 || p.Data[0] != 2 {
				t.Fatalf("want result/PTS/data=1024/1024/2; got %d/%d/%d", result.Timestamp, p.PTS.Value, p.Data[0])
			}
		})
	}
}

// A current demuxer can block on its own source (DASH SingleFile uses HTTPSource).
type auditBlockingDemux struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (d *auditBlockingDemux) Streams() []Stream { return nil }
func (d *auditBlockingDemux) Info() Info        { return Info{} }
func (d *auditBlockingDemux) Seek(context.Context, SeekRequest) (SeekResult, error) {
	return SeekResult{}, nil
}
func (d *auditBlockingDemux) ReadPacket(ctx context.Context) (*Packet, error) {
	close(d.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.closed:
		return nil, ErrClosed
	}
}
func (d *auditBlockingDemux) Close() error { d.once.Do(func() { close(d.closed) }); return nil }
func TestAuditDASHCloseUnblocksRead(t *testing.T) {
	inner := &auditBlockingDemux{entered: make(chan struct{}), closed: make(chan struct{})}
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &dashDemuxer{root: root, cancel: cancel, current: inner}
	readDone := make(chan struct{})
	go func() { d.ReadPacket(context.Background()); close(readDone) }()
	<-inner.entered
	closeDone := make(chan struct{})
	go func() { d.Close(); close(closeDone) }()
	select {
	case <-closeDone:
	case <-time.After(100 * time.Millisecond):
		inner.Close()
		<-readDone
		<-closeDone
		t.Fatal("Close waited for opMu while ReadPacket waited for current.Close")
	}
	<-readDone
}
func TestAuditProgressiveTrackDelayDTS(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "delay-*.mp4")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	m, e := NewMuxer(f, MuxOptions{Format: FormatMP4})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if _, e = m.AddStream(auditAACStream()); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		ts := int64(i) * 48000
		e = m.WritePacket(context.Background(), &Packet{StreamIndex: i, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(1024), Flags: PacketKeyframe, Data: []byte{1}})
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(f.Name())
	if e != nil {
		t.Fatal(e)
	}
	d, e := Open(context.Background(), MemorySource("a.mp4", data), OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	for i := 0; i < 2; i++ {
		p, e := d.ReadPacket(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if p.StreamIndex == 1 && (p.DTS.Value != 48000 || p.PTS.Value != 48000) {
			t.Errorf("delayed audio expected DTS=PTS=48000, got DTS=%d PTS=%d", p.DTS.Value, p.PTS.Value)
		}
		p.Release()
	}
}
func TestAuditMP4OpusToWebM(t *testing.T) {
	// RFC7845 LE OpusHead: version1 stereo, pre-skip312, rate48000, gain0, mapping0.
	head := append([]byte("OpusHead"), 1, 2, 0x38, 1, 0x80, 0xbb, 0, 0, 0, 0, 0)
	cfg, e := opus.DOPSFromHead(head)
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	m, e := NewMuxer(&b, MuxOptions{Format: FormatWebM})
	if e != nil {
		t.Fatal(e)
	}
	_, e = m.AddStream(Stream{Type: MediaAudio, Codec: CodecOpus, TimeBase: Rational{Num: 1, Den: 48000}, SampleRate: 48000, Channels: 2, Config: CodecConfig{Format: CodecConfigDOPS, Data: cfg}})
	if e != nil {
		t.Fatalf("MP4-demuxed Opus config cannot be remuxed to WebM: %v", e)
	}
}
func TestAuditManifestEOFStable(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := &auditEOF{closed: false}
	d := &hlsDemuxer{root: root, cancel: cancel, current: inner, playlist: manifest.HLSPlaylist{EndList: true}, segments: []manifest.HLSSegment{{}}}
	_, e := d.ReadPacket(context.Background())
	if !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
	_, e = d.ReadPacket(context.Background())
	if !errors.Is(e, io.EOF) {
		t.Fatalf("second EOF read = %v", e)
	}
}

type auditEOF struct{ closed bool }

func (d *auditEOF) Streams() []Stream { return nil }
func (d *auditEOF) Info() Info        { return Info{} }
func (d *auditEOF) ReadPacket(context.Context) (*Packet, error) {
	if d.closed {
		return nil, ErrClosed
	}
	return nil, io.EOF
}
func (d *auditEOF) Seek(context.Context, SeekRequest) (SeekResult, error) { return SeekResult{}, nil }
func (d *auditEOF) Close() error                                          { d.closed = true; return nil }
func TestAuditLiveBufferSaturationMakesProgress(t *testing.T) {
	dst := &captureMuxer{}
	l, e := NewLiveMuxer(dst, LiveIngestOptions{MaxBufferPackets: 2, JitterWindow: 400 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	index, e := l.AddStream(auditAACStream())
	if e != nil {
		t.Fatal(e)
	}
	for i := int64(0); i < 100; i++ {
		ts := i * 1024
		e = l.WritePacket(context.Background(), &Packet{StreamIndex: index, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(1024), Data: []byte{1}})
		if e != nil {
			t.Fatal(e)
		}
	}
	metrics, _ := l.Metrics(index)
	if len(dst.packets) == 0 {
		t.Fatalf("100 ordered packets produced zero output; overflow drops=%d", metrics.DroppedOverflow)
	}
}
func TestAuditManifestRedirectBase(t *testing.T) {
	data := auditSegment(t, 0)
	for _, kind := range []string{"hls", "dash"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/start":
					http.Redirect(w, r, "/dir/index", http.StatusFound)
				case "/dir/index":
					if kind == "hls" {
						fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\ns.mp4\n#EXT-X-ENDLIST\n")
					} else {
						fmt.Fprint(w, `<MPD type="static"><Period><AdaptationSet><Representation id="a"><SegmentList duration="1"><SegmentURL media="s.mp4"/></SegmentList></Representation></AdaptationSet></Period></MPD>`)
					}
				case "/dir/s.mp4":
					w.Write(data)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			var d Demuxer
			var e error
			if kind == "hls" {
				d, e = OpenHLS(context.Background(), srv.URL+"/start", HLSOptions{})
			} else {
				d, e = OpenDASH(context.Background(), srv.URL+"/start", DASHOptions{})
			}
			if e != nil {
				t.Fatalf("relative URI after redirect failed: %v", e)
			}
			d.Close()
		})
	}
}
func TestAuditTSVideoIdentityRemux(t *testing.T) {
	// Reuse existing MSB-first H.264 SPS/PPS/IDR fixture from live_test.go.
	var src bytes.Buffer
	m, e := NewMuxer(&src, MuxOptions{Format: FormatMPEGTS})
	if e != nil {
		t.Fatal(e)
	}
	idx, e := m.AddStream(Stream{Type: MediaVideo, Codec: CodecH264, TimeBase: Rational{Num: 1, Den: 90000}, Width: 16, Height: 16})
	if e != nil {
		t.Fatal(e)
	}
	for i := int64(0); i < 2; i++ {
		ts := i * 3000
		e = m.WritePacket(context.Background(), &Packet{StreamIndex: idx, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(3000), Flags: PacketKeyframe, Data: liveH264AU(true, true, 0, 0, byte(i), true)})
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	e = Remux(context.Background(), []RemuxInput{{Source: MemorySource("v.ts", src.Bytes())}}, &out, MuxOptions{Format: FormatMPEGTS})
	if e != nil {
		t.Fatalf("library's own TS video cannot remux to TS: %v", e)
	}
}
func TestAuditMP4SeekExactTick(t *testing.T) {
	var data bytes.Buffer
	m, e := NewMuxer(&data, MuxOptions{Format: FormatMP4})
	if e != nil {
		t.Fatal(e)
	}
	m.AddStream(auditAACStream())
	for i := int64(0); i < 3; i++ {
		ts := i * 1024
		e = m.WritePacket(context.Background(), &Packet{StreamIndex: 0, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(1024), Flags: PacketKeyframe, Data: []byte{1}})
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	d, e := Open(context.Background(), MemorySource("a.mp4", data.Bytes()), OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	result, e := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 1024})
	if e != nil {
		t.Fatal(e)
	}
	p, e := d.ReadPacket(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Release()
	if result.Timestamp != p.PTS.Value {
		t.Fatalf("seek reported tick %d but selected packet has tick %d", result.Timestamp, p.PTS.Value)
	}
}
func TestAuditMP4FLACToMatroska(t *testing.T) {
	// RFC9639 STREAMINFO: block sizes16; 48kHz20-bit, stereo stored1,
	// 16-bit stored15, total samples0. MP4 dfLa FullBox flags0, last STREAMINFO34.
	si := make([]byte, 34)
	si[1] = 16
	si[3] = 16
	packed := uint64(48000)<<44 | uint64(1)<<41 | uint64(15)<<36
	for i := 0; i < 8; i++ {
		si[10+i] = byte(packed >> uint(56-i*8))
	}
	dfla := append([]byte{0, 0, 0, 0, 0x80, 0, 0, 34}, si...)
	var b bytes.Buffer
	m, e := NewMuxer(&b, MuxOptions{Format: FormatMatroska})
	if e != nil {
		t.Fatal(e)
	}
	_, e = m.AddStream(Stream{Type: MediaAudio, Codec: CodecFLAC, TimeBase: Rational{Num: 1, Den: 48000}, SampleRate: 48000, Channels: 2, Config: CodecConfig{Format: CodecConfigFLACStreamInfo, Data: dfla}})
	if e != nil {
		t.Fatalf("MP4-demuxed FLAC dfLa cannot be remuxed to Matroska: %v", e)
	}
}
func TestAuditProgressiveLongTrackDelayOverflow(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "longdelay-*.mp4")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	m, e := NewMuxer(f, MuxOptions{Format: FormatMP4})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if _, e = m.AddStream(auditAACStream()); e != nil {
			t.Fatal(e)
		}
	}
	want := int64(5*24*60*60) * 48000
	for i := 0; i < 2; i++ {
		ts := int64(i) * want
		e = m.WritePacket(context.Background(), &Packet{StreamIndex: i, PTS: KnownTimestamp(ts), DTS: KnownTimestamp(ts), Duration: KnownTimestamp(1024), Flags: PacketKeyframe, Data: []byte{1}})
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(f.Name())
	if e != nil {
		t.Fatal(e)
	}
	d, e := Open(context.Background(), MemorySource("long.mp4", data), OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	for i := 0; i < 2; i++ {
		p, e := d.ReadPacket(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if p.StreamIndex == 1 && p.PTS.Value != want {
			t.Errorf("five-day delay expected PTS=%d, got %d", want, p.PTS.Value)
		}
		p.Release()
	}
}
