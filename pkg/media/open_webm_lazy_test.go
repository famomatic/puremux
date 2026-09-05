package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"testing"
)

type webMStartupSpool struct {
	*bytes.Reader
	available int64
	maxRead   int64
}

func (s *webMStartupSpool) Name() string               { return "spooled.webm" }
func (s *webMStartupSpool) Close() error               { return nil }
func (s *webMStartupSpool) Read(p []byte) (int, error) { return s.ReadContext(context.Background(), p) }
func (s *webMStartupSpool) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	if pos+int64(len(p)) > s.available {
		return 0, errors.New("spool tail unavailable")
	}
	n, err := s.Reader.Read(p)
	s.maxRead = max(s.maxRead, pos+int64(n))
	return n, err
}
func (s *webMStartupSpool) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 0, errors.New("SeekEnd waits for complete download")
	}
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	target := offset
	if whence == io.SeekCurrent {
		target += pos
	}
	if target > s.available {
		return pos, errors.New("spool tail unavailable")
	}
	return s.Reader.Seek(offset, whence)
}

func TestOpenWebMContextSpoolReturnsFirstPacketBeforeTail(t *testing.T) {
	// Independent libwebm golden fixture, unchanged bytes. Cluster header=12,
	// Timestamp E7 81 00=3, first BlockGroup A0 97=25 (big-endian EBML VINT).
	encoded, err := os.ReadFile("../../internal/format/webm/testdata/libwebm_discard_padding.webm.b64")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	cluster := bytes.LastIndex(data, []byte{0x1f, 0x43, 0xb6, 0x75})
	source := &webMStartupSpool{Reader: bytes.NewReader(data), available: int64(cluster + 12 + 3 + 25)}
	d, err := Open(context.Background(), source, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Info().Format != FormatWebM || len(d.Streams()) != 1 {
		t.Fatal("startup metadata missing")
	}
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if len(p.Data) != 10 || p.DiscardPadding != 12_810_000 {
		t.Fatalf("packet=%+v", p)
	}
	if source.maxRead > source.available || source.maxRead >= int64(len(data)) {
		t.Fatal("Open required complete spool")
	}
}

func TestLazyWebMTrailingTagsRemainVisibleAndCannotBeSilentlyLost(t *testing.T) {
	data := makeRemuxWebM(t, []byte{0xf8, 0x55})
	// RFC 9559 Tags(Tag(SimpleTag(TagName="TITLE",TagString="Late"))).
	// MSB-first EBML VINT sizes: 95=21, 92=18, 8F=15, 85=5, 84=4.
	data = append(data, 0x12, 0x54, 0xc3, 0x67, 0x95, 0x73, 0x73, 0x92, 0x67, 0xc8, 0x8f,
		0x45, 0xa3, 0x85, 'T', 'I', 'T', 'L', 'E', 0x44, 0x87, 0x84, 'L', 'a', 't', 'e')
	d, err := Open(context.Background(), MemorySource("late.webm", data), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Info().Metadata["title"] != "" {
		t.Fatal("eager tags")
	}
	for {
		p, err := d.ReadPacket(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		p.Release()
	}
	if d.Info().Metadata["title"] != "Late" {
		t.Fatal("late tags missing")
	}
	for _, allow := range []bool{false, true} {
		err := Remux(context.Background(), []RemuxInput{{Source: MemorySource("late.webm", data)}}, io.Discard, MuxOptions{Format: FormatMP4, AllowMetadataLoss: allow})
		if allow && err != nil {
			t.Fatal(err)
		}
		if !allow && !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("silent metadata loss: %v", err)
		}
	}
}
