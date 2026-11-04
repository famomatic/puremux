package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAudit20260906OutputDirectoryPreserved(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "existing.mp4")
	temp := filepath.Join(root, "new.tmp")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "original"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	err := installRemuxOutput(temp, output)
	info, statErr := os.Stat(output)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("output directory replaced: install=%v stat=%v info=%v", err, statErr, info)
	}
}

func TestAudit20260906ManifestExactOrigin(t *testing.T) {
	data := auditSegment(t, 1024) // Existing MSB-first AAC 11 90 fixture; sample payload opaque.
	for _, kind := range []string{"hls", "dash"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/index" {
					w.Write(data)
					return
				}
				if kind == "hls" {
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\ns.mp4\n#EXT-X-ENDLIST\n")
				} else {
					fmt.Fprint(w, `<MPD type="static" mediaPresentationDuration="PT1S"><Period><AdaptationSet contentType="audio"><Representation id="a"><SegmentList timescale="1" duration="1"><SegmentURL media="s.mp4"/></SegmentList></Representation></AdaptationSet></Period></MPD>`)
				}
			}))
			defer srv.Close()
			var d Demuxer
			var err error
			if kind == "hls" {
				d, err = OpenHLS(context.Background(), srv.URL+"/index", HLSOptions{})
			} else {
				d, err = OpenDASH(context.Background(), srv.URL+"/index", DASHOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			p, err := d.ReadPacket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Release()
			if p.PTS.Value != 0 {
				t.Fatalf("first packet PTS=%d, want 0", p.PTS.Value)
			}
		})
	}
}

func TestAudit20260906InitErrorChain(t *testing.T) {
	data := auditSegment(t, 0)
	moof := bytes.Index(data, []byte("moof")) - 4
	src := &failingOpenSource{Reader: bytes.NewReader(make([]byte, 128)), cause: context.DeadlineExceeded}
	_, err := OpenMP4WithInit(context.Background(), data[:moof], src)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline: %v", err)
	}
}

func TestAudit20260906OggReadRetry(t *testing.T) {
	// RFC7845 little endian OpusHead (pre-skip 0, 48000Hz); F8=CELT20ms.
	head := append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	data := append(mediaTestOggPage(2, 0, 1, 0, head), mediaTestOggPage(0, 0, 1, 1, mediaTestOpusTags("audit"))...)
	start := len(data)
	data = append(data, mediaTestOggPage(4, 960, 1, 2, []byte{0xf8, 0xff, 0xfe})...)
	src := &audit20260906TransientSource{Reader: bytes.NewReader(data), failureAt: -1}
	d, err := Open(context.Background(), src, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	src.failureAt = int64(start + 27) // Fixed header consumed; fail once before lacing/body.
	if p, err := d.ReadPacket(context.Background()); err == nil {
		p.Release()
		t.Fatal("fault not reached")
	}
	pos, _ := src.Reader.Seek(0, io.SeekCurrent)
	t.Logf("cursor after failed read=%d, page start=%d", pos, start)
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("retry after transport recovery: %v", err)
	}
	p.Release()
}

func TestAudit20260906HTTPBodyRetry(t *testing.T) {
	calls := 0
	retries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start, end, _ := testRange(r.Header.Get("Range"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/10", start, end))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(206)
		if calls == 2 {
			w.Write([]byte("0"))
			return
		}
		w.Write([]byte("0123456789")[start : end+1])
	}))
	defer srv.Close()
	s, err := OpenHTTP(context.Background(), srv.URL, HTTPSourceOptions{ReadAheadBytes: -1, MaxRetries: 1, RetryPolicy: func(int, int, error) bool { retries++; return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.ReadAt(make([]byte, 4), 0)
	if err != nil {
		t.Fatalf("transient body failure not retried: err=%v retries=%d", err, retries)
	}
}

func TestAudit20260906HTTPCanceledQueuedRead(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, _ := testRange(r.Header.Get("Range"))
		if end > 0 {
			close(entered)
			<-release
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/10", start, end))
		w.WriteHeader(206)
		w.Write([]byte("0123456789")[start : end+1])
	}))
	defer srv.Close()
	s, err := OpenHTTP(context.Background(), srv.URL, HTTPSourceOptions{ReadAheadBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan struct{})
	go func() { s.Read(make([]byte, 2)); close(done) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() { _, err := s.ReadContext(ctx, make([]byte, 1)); second <- err }()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%v", err)
		}
		close(release)
		<-done
	case <-time.After(100 * time.Millisecond):
		close(release)
		<-done
		<-second
		t.Fatal("already-canceled ReadContext waits behind unrelated network read")
	}
}

func TestAudit20260906HLSFailedSeekKeepsPosition(t *testing.T) {
	data := auditSegment(t, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\na.mp4\n#EXTINF:1,\nb.mp4\n#EXT-X-ENDLIST\n")
		case "/b.mp4":
			w.WriteHeader(503)
		default:
			w.Write(data)
		}
	}))
	defer srv.Close()
	d, err := OpenHLS(context.Background(), srv.URL+"/index", HLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: -1, Target: int64(time.Second)}); err == nil {
		t.Fatal("fault not reached")
	}
	p, err = d.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("lost previous playable segment after failed seek: %v", err)
	}
	defer p.Release()
	if p.PTS.Value != 1024 {
		t.Fatalf("resume PTS=%d", p.PTS.Value)
	}
}

type audit20260906TransientSource struct {
	*bytes.Reader
	failureAt int64
}

func (s *audit20260906TransientSource) Name() string { return "transient.ogg" }
func (s *audit20260906TransientSource) Close() error { return nil }
func (s *audit20260906TransientSource) Read(p []byte) (int, error) {
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	if pos == s.failureAt {
		s.failureAt = -1
		return 0, context.DeadlineExceeded
	}
	return s.Reader.Read(p)
}

func TestAudit20260906AACChannelConfiguration7(t *testing.T) {
	// ISO14496-3 channel_configuration 7 means eight channels, not seven.
	// ADTS MSB-first: sync fff, MPEG4/layer0/noCRC=1, profile LC=01,
	// frequency index3=0011, channel_config=111, length8, fullness7ff, blocks0.
	data := []byte{0xff, 0xf1, 0x4d, 0xc0, 0x01, 0x1f, 0xfc, 0x01}
	d, err := Open(context.Background(), MemorySource("8ch.aac", data), OpenOptions{FormatHint: FormatADTS})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if channels := d.Streams()[0].Channels; channels != 8 {
		t.Fatalf("7.1 ADTS reported %d channels", channels)
	}
}
func TestAudit20260906AAC8ChannelMux(t *testing.T) {
	// ASC AOT2=00010, frequency3=0011, channel config7=0111 -> 11 b8.
	var out bytes.Buffer
	m, err := NewMuxer(&out, MuxOptions{Format: FormatMP4})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := auditAACStream()
	s.Channels = 8
	s.Config.Data = []byte{0x11, 0xb8}
	if _, err = m.AddStream(s); err != nil {
		t.Fatalf("valid 8-channel AAC config rejected: %v", err)
	}
}
func TestAudit20260906ADTSProfileChange(t *testing.T) {
	// Same indexed48kHz/stereo, ADTS profile changes from LC(01) to Main(00).
	data := []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x1f, 0xfc, 1, 0xff, 0xf1, 0x0c, 0x80, 1, 0x1f, 0xfc, 2}
	d, err := Open(context.Background(), MemorySource("profile.aac", data), OpenOptions{FormatHint: FormatADTS})
	if err == nil {
		defer d.Close()
		t.Fatal("AAC profile change accepted with stale ASC")
	}
}

func TestDASHFailedSeekKeepsPosition(t *testing.T) {
	data := auditSegment(t, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index":
			fmt.Fprint(w, `<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet contentType="audio"><Representation id="a"><SegmentList timescale="1" duration="1"><SegmentURL media="a.mp4"/><SegmentURL media="b.mp4"/></SegmentList></Representation></AdaptationSet></Period></MPD>`)
		case "/b.mp4":
			w.WriteHeader(503)
		default:
			w.Write(data)
		}
	}))
	defer srv.Close()
	d, err := OpenDASH(context.Background(), srv.URL+"/index", DASHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: -1, Target: int64(time.Second)}); err == nil {
		t.Fatal("fault not reached")
	}
	p, err = d.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("lost previous playable segment after failed seek: %v", err)
	}
	defer p.Release()
	if p.PTS.Value != 1024 {
		t.Fatalf("resume PTS=%d", p.PTS.Value)
	}
}
