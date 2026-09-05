package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type auditDownloadPrefix struct {
	*bytes.Reader
	available int64
}

func (s *auditDownloadPrefix) Name() string { return "download" }
func (s *auditDownloadPrefix) Close() error { return nil }
func (s *auditDownloadPrefix) Read(p []byte) (int, error) {
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	if pos+int64(len(p)) > s.available {
		return 0, errors.New("tail not downloaded")
	}
	return s.Reader.Read(p)
}
func TestIntegrationAuditOggStartsBeforeTail(t *testing.T) {
	// RFC7845 little-endian OpusHead, pre-skip0, rate48000; RFC6716 F8 =20ms.
	head := append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	data := append(mediaTestOggPage(2, 0, 1, 0, head), mediaTestOggPage(0, 0, 1, 1, mediaTestOpusTags("audit"))...)
	data = append(data, mediaTestOggPage(0, 960, 1, 2, []byte{0xf8, 0xff, 0xfe})...)
	available := len(data)
	data = append(data, mediaTestOggPage(4, 1920, 1, 3, []byte{0xf8, 0xff, 0xfe})...)
	src := &auditDownloadPrefix{Reader: bytes.NewReader(data), available: int64(available)}
	d, err := Open(context.Background(), src, OpenOptions{})
	if err != nil {
		t.Fatalf("head/tags/first page available: %v", err)
	}
	defer d.Close()
	if d.Info().DurationKnown {
		t.Fatal("duration known before EOS")
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 0}); err == nil {
		t.Fatal("index unexpectedly available")
	}
	src.available = int64(len(data))
	p, err = d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if info := d.Info(); !info.DurationKnown || info.Duration != 40*time.Millisecond {
		t.Fatalf("EOS duration: %+v", info)
	}

}
