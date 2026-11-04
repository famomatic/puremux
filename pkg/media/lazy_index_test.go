package media

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

type prefixAuditSource struct {
	*bytes.Reader
	limit int64
}

func (s *prefixAuditSource) Name() string { return "prefix" }
func (s *prefixAuditSource) Close() error { return nil }
func (s *prefixAuditSource) Read(p []byte) (int, error) {
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	if pos >= s.Reader.Size() {
		return 0, io.EOF
	}
	if pos >= s.limit {
		return 0, context.DeadlineExceeded
	}
	return s.Reader.Read(p[:min(int64(len(p)), s.limit-pos)])
}
func (s *prefixAuditSource) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.Read(p)
}
func TestLazyRawHeaderStartupAndFailedSeek(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x1f, 0xfc, 1}
	mp3 := append([]byte{0xff, 0xfb, 0x90, 0x64}, make([]byte, 413)...)
	for _, tc := range []struct {
		format   Format
		frame    []byte
		duration int64
	}{{FormatADTS, adts, 1024}, {FormatMP3, mp3, 1152}} {
		t.Run(tc.format.String(), func(t *testing.T) {
			data := bytes.Repeat(tc.frame, 3)
			src := &prefixAuditSource{Reader: bytes.NewReader(data), limit: int64(len(tc.frame))}
			d, err := Open(context.Background(), src, OpenOptions{FormatHint: tc.format, LazyIndex: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if d.Info().DurationKnown {
				t.Fatal("duration claimed before tail")
			}
			p, err := d.ReadPacket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p.Release()
			if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: tc.duration}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("seek=%v", err)
			}
			src.limit = int64(len(data))
			p, err = d.ReadPacket(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if p.PTS.Value != tc.duration {
				t.Fatal(p.PTS)
			}
			p.Release()
			result, err := d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 0})
			if err != nil || result.Timestamp != 0 {
				t.Fatalf("seek=%v %v", result, err)
			}
		})
	}
}
func TestLazyFLACHeaderStartup(t *testing.T) {
	data, _ := hex.DecodeString("664c6143800000221000100000000f00000f0ac442f0000000013e84b41807dc690307586a3dad1a2e0ffff869180000bf0358fd03128baa9a")
	src := &prefixAuditSource{Reader: bytes.NewReader(data), limit: 42}
	d, err := Open(context.Background(), src, OpenOptions{LazyIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	src.limit = int64(len(data))
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Data) != 15 {
		t.Fatal(len(p.Data))
	}
	p.Release()
	if _, err = d.ReadPacket(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !d.Info().DurationKnown {
		t.Fatal("missing EOF duration")
	}
}
func TestLazySeekableTS(t *testing.T) {
	var data bytes.Buffer
	m, err := NewMuxer(&data, MuxOptions{Format: FormatMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.AddStream(Stream{Type: MediaAudio, Codec: CodecAAC, SampleRate: 48000, Channels: 2, TimeBase: Rational{1, 48000}})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 3; i++ {
		err = m.WritePacket(context.Background(), &Packet{StreamIndex: id, Data: []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x1f, 0xfc, 1}, PTS: KnownTimestamp(i * 1024), DTS: KnownTimestamp(i * 1024), Duration: KnownTimestamp(1024)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	src := &prefixAuditSource{Reader: bytes.NewReader(data.Bytes()), limit: 564}
	d, err := Open(context.Background(), src, OpenOptions{FormatHint: FormatMPEGTS, LazyIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pts := p.PTS.Value
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: 0}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	src.limit = int64(data.Len())
	p, err = d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PTS.Value != pts+1024 {
		t.Fatalf("lost resume %d/%d", pts, p.PTS.Value)
	}
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: 0}); err != nil {
		t.Fatal(err)
	}
}
