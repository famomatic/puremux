package mp4

import (
	"bytes"
	"github.com/famomatic/puremux/internal/core"
	"io"
	"os"
	"testing"
)

func TestAuditEmptySTSSIsNotAllSync(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "nosync-*.mp4")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	w, _ := NewProgressiveWriter(f)
	_, e = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90000, Width: 16, Height: 16, ConfigType: "avcC", Config: testAVCC()})
	if e != nil {
		t.Fatal(e)
	}
	if e = w.WriteSample(OutputSample{TrackID: 1, DTS: 0, PTS: 0, Duration: 3000, Keyframe: false, Data: []byte{0, 0, 0, 1, 0x41}}); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(f.Name())
	if !bytes.Contains(data, []byte{'s', 't', 's', 's', 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("missing spec-derived empty stss FullBox: version/flags=0, entry_count=0")
	}
	f.Seek(0, io.SeekStart)
	r, e := NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	p, e := r.NextSample()
	if e != nil {
		t.Fatal(e)
	}
	if p.Keyframe {
		t.Fatal("empty stss incorrectly interpreted as all-sync")
	}
}

type auditReadCounter struct {
	*bytes.Reader
	read int64
}

func (r *auditReadCounter) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.read += int64(n)
	return n, e
}
func TestAuditMP4OpenReadsMDAT(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "read-*.mp4")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	w, _ := NewProgressiveWriter(f)
	_, e = w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecAAC, TimeScale: 48000, Channels: 2, SampleRate: 48000, ConfigType: "asc", Config: []byte{0x11, 0x90}})
	if e != nil {
		t.Fatal(e)
	}
	if e = w.WriteSample(OutputSample{TrackID: 1, Duration: 1024, Data: make([]byte, 1<<20)}); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(f.Name())
	r := &auditReadCounter{Reader: bytes.NewReader(data)}
	if _, e = NewReader(r); e != nil {
		t.Fatal(e)
	}
	if r.read >= 1<<20 {
		t.Fatalf("Open read %d bytes for %d-byte file before requesting a sample", r.read, len(data))
	}
}
