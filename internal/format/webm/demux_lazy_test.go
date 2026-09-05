package webm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"testing"
)

var errSpoolUnavailable = errors.New("spool bytes not downloaded")

type gatedSpool struct {
	*bytes.Reader
	available int64
	furthest  int64
}

func (s *gatedSpool) Read(p []byte) (int, error) {
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	if s.Reader.Len() == 0 {
		return 0, io.EOF
	}
	if pos+int64(len(p)) > s.available {
		return 0, errSpoolUnavailable
	}
	n, err := s.Reader.Read(p)
	s.furthest = max(s.furthest, pos+int64(n))
	return n, err
}
func (s *gatedSpool) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 0, errSpoolUnavailable
	}
	pos, _ := s.Reader.Seek(0, io.SeekCurrent)
	target := off
	if whence == io.SeekCurrent {
		target += pos
	}
	if target > s.available {
		return pos, errSpoolUnavailable
	}
	return s.Reader.Seek(off, whence)
}

func lazyWebMFixture() ([]byte, int) {
	// RFC 8794 VINTs are MSB-first: FF unknown size, 87=7, 9B=27.
	// RFC 9559: Segment 18538067, Info 1549A966, Tracks 1654AE6B,
	// Cluster 1F43B675, E7 Timestamp, A3 SimpleBlock. Big-endian scale
	// 0F4240=1,000,000 ns. Track 1 VP8, 16x16. Block 81 0000 80:
	// VINT track=1, signed relative timestamp=0, key flag=1, no lacing.
	data := []byte{
		0x1a, 0x45, 0xdf, 0xa3, 0x87, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm',
		0x18, 0x53, 0x80, 0x67, 0xff,
		0x15, 0x49, 0xa9, 0x66, 0x87, 0x2a, 0xd7, 0xb1, 0x83, 0x0f, 0x42, 0x40,
		0x16, 0x54, 0xae, 0x6b, 0x9b, 0xae, 0x99,
		0xd7, 0x81, 1, 0x73, 0xc5, 0x81, 1, 0x83, 0x81, 1, 0x86, 0x85, 'V', '_', 'V', 'P', '8',
		0xe0, 0x86, 0xb0, 0x81, 16, 0xba, 0x81, 16,
		0x1f, 0x43, 0xb6, 0x75, 0xff, 0xe7, 0x81, 0,
		0xa3, 0x85, 0x81, 0, 0, 0x80, 0x11,
	}
	available := len(data)
	// A 1 MiB Void is legal inside the unknown-size Cluster. 30 00 00 is
	// a 3-byte size VINT with value 0x100000, not an unknown size.
	data = append(data, 0xec, 0x30, 0, 0)
	data = append(data, make([]byte, 1<<20)...)
	data = append(data, 0x1f, 0x43, 0xb6, 0x75, 0x8b, 0xe7, 0x82, 0x03, 0xe8, 0xa3, 0x85, 0x81, 0, 0, 0x80, 0x22)
	return data, available
}

func TestDemuxLazySpoolStartupAndSeek(t *testing.T) {
	data, available := lazyWebMFixture()
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown length", true: "known length"}[known], func(t *testing.T) {
			spool := &gatedSpool{Reader: bytes.NewReader(data), available: int64(available)}
			size := int64(-1)
			if known {
				size = int64(len(data))
			}
			rd, err := NewDemuxReaderWithSize(spool, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(rd.clusters) != 0 || rd.indexComplete {
				t.Fatal("Open indexed media")
			}
			p, err := rd.NextPacket(context.Background())
			if err != nil || p.TimestampNS != 0 || !bytes.Equal(p.Data, []byte{0x11}) {
				t.Fatalf("first packet: %+v %v", p, err)
			}
			t.Logf("first packet: accessed %d/%d bytes", spool.furthest, len(data))
			if spool.furthest > int64(available) {
				t.Fatal("startup read undownloaded tail")
			}
			if _, err = rd.SeekTicks(context.Background(), 1000, 1); !errors.Is(err, errSpoolUnavailable) {
				t.Fatalf("seek unavailable tail: %v", err)
			}
			spool.available = int64(len(data))
			// Failed indexing restored the packet cursor; normal reading must resume.
			p, err = rd.NextPacket(context.Background())
			if err != nil || p.TimestampNS != 1_000_000_000 || !bytes.Equal(p.Data, []byte{0x22}) {
				t.Fatalf("resume: %+v %v", p, err)
			}
			actual, err := rd.SeekTicks(context.Background(), 1000, 1)
			if err != nil || actual != 1000 {
				t.Fatalf("seek: %d %v", actual, err)
			}
			p, err = rd.NextPacket(context.Background())
			if err != nil || p.Data[0] != 0x22 {
				t.Fatalf("seek packet: %+v %v", p, err)
			}
			if len(rd.clusters) != 2 {
				t.Fatalf("duplicate indexes: %d", len(rd.clusters))
			}
		})
	}
}

func TestDemuxLazyLibwebmGolden(t *testing.T) {
	encoded, err := os.ReadFile("testdata/libwebm_discard_padding.webm.b64")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	// Independent libwebm fixture: actual Cluster begins at 273, 12-byte
	// header, E7 81 00 timestamp, A0 97 first BlockGroup (25 bytes).
	// Only the prefix through that first group is downloaded.
	cluster := bytes.LastIndex(data, []byte{0x1f, 0x43, 0xb6, 0x75})
	available := cluster + 12 + 3 + 25
	spool := &gatedSpool{Reader: bytes.NewReader(data), available: int64(available)}
	rd, err := NewDemuxReader(spool)
	if err != nil {
		t.Fatal(err)
	}
	p, err := rd.NextPacket(context.Background())
	if err != nil || p.DiscardPaddingNS != 12_810_000 {
		t.Fatalf("golden: %+v %v", p, err)
	}
	if spool.furthest >= int64(len(data)) {
		t.Fatal("golden tail visited")
	}
}

func TestDemuxLazyTrailingIndexAndTags(t *testing.T) {
	data, _ := lazyWebMFixture()
	first := bytes.Index(data, []byte{0x1f, 0x43, 0xb6, 0x75})
	last := bytes.LastIndex(data, []byte{0x1f, 0x43, 0xb6, 0x75})
	var cues bytes.Buffer
	// Segment payload starts at 17 (12-byte EBML header, 5-byte Segment).
	for _, cue := range []CuePoint{{Timecode: 0, Track: 1, ClusterPosition: uint64(first - 17)}, {Timecode: 1000, Track: 1, ClusterPosition: uint64(last - 17)}} {
		if err := writeCuePoint(&cues, cue); err != nil {
			t.Fatal(err)
		}
	}
	var tail bytes.Buffer
	if err := writeElement(&tail, idCues, cues.Bytes()); err != nil {
		t.Fatal(err)
	}
	// SimpleTag -> Tag -> Tags; UTF-8 TagName/TagString are RFC 9559 elements.
	var simple, tag, tags bytes.Buffer
	_ = writeString(&simple, idTagName, "TITLE")
	_ = writeString(&simple, idTagString, "Late title")
	_ = writeElement(&tag, idSimpleTag, simple.Bytes())
	_ = writeElement(&tags, idTag, tag.Bytes())
	_ = writeElement(&tail, idTags, tags.Bytes())
	data = append(data, tail.Bytes()...)
	rd, err := NewDemuxReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	immediate, err := NewDemuxReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := immediate.SeekTicksWithFlags(context.Background(), 500, 1, false, false)
	if err != nil || actual != 1000 || len(immediate.cues) != 2 {
		t.Fatalf("immediate seek: %d %v", actual, err)
	}
	packet, err := immediate.NextPacket(context.Background())
	if err != nil || packet.Data[0] != 0x22 {
		t.Fatalf("cue offset: %+v %v", packet, err)
	}

	if len(rd.cues) != 0 || rd.Metadata().Tags["title"] != "" {
		t.Fatal("tail eagerly indexed")
	}
	for {
		_, err := rd.NextPacket(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(rd.cues) != 2 || rd.Metadata().Tags["title"] != "Late title" {
		t.Fatalf("late metadata: %v %+v", rd.cues, rd.Metadata())
	}
	for _, backward := range []bool{false, true} {
		actual, err := rd.SeekTicksWithFlags(context.Background(), 500, 1, backward, false)
		want := uint64(1000)
		if backward {
			want = 0
		}
		if err != nil || actual != want {
			t.Fatalf("seek backward=%v: %d %v", backward, actual, err)
		}
	}
	if len(rd.clusters) != 2 || len(rd.cues) != 2 {
		t.Fatal("duplicate index after rereading")
	}
}

func TestDemuxLazyFailedSeekPreservesLacing(t *testing.T) {
	data, available := lazyWebMFixture()
	// Fixed-size lacing flag=0x04, count-minus-one=1, two one-byte frames.
	block := []byte{0xa3, 0x87, 0x81, 0, 0, 0x84, 1, 0x11, 0x12}
	data = append(append(append([]byte(nil), data[:available-7]...), block...), data[available:]...)
	spool := &gatedSpool{Reader: bytes.NewReader(data), available: int64(available + 2)}
	rd, err := NewDemuxReader(spool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := rd.NextPacket(context.Background())
	if err != nil || first.Data[0] != 0x11 {
		t.Fatal(err)
	}
	if _, err = rd.SeekTicks(context.Background(), 1000, 1); !errors.Is(err, errSpoolUnavailable) {
		t.Fatal(err)
	}
	next, err := rd.NextPacket(context.Background())
	if err != nil || next.Data[0] != 0x12 {
		t.Fatalf("lost pending lace: %+v %v", next, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = rd.SeekTicks(ctx, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDemuxLazyTruncatedPacket(t *testing.T) {
	data, available := lazyWebMFixture()
	for _, cut := range []int{available - 1, available - 3} {
		rd, err := NewDemuxReaderWithSize(bytes.NewReader(data[:cut]), -1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = rd.NextPacket(context.Background()); err == nil {
			t.Fatal("truncated block accepted")
		}
	}
}
