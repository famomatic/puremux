package puremux

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// seekBuf is an in-memory io.WriteSeeker for seekable-mode tests.
type seekBuf struct {
	buf []byte
	pos int
}

func (s *seekBuf) Write(p []byte) (int, error) {
	if s.pos+len(p) > len(s.buf) {
		s.buf = append(s.buf, make([]byte, s.pos+len(p)-len(s.buf))...)
	}
	copy(s.buf[s.pos:], p)
	s.pos += len(p)
	return len(p), nil
}

func (s *seekBuf) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case 0:
		abs = offset
	case 1:
		abs = int64(s.pos) + offset
	case 2:
		abs = int64(len(s.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	s.pos = int(abs)
	return abs, nil
}

func (s *seekBuf) Bytes() []byte { return s.buf }

func TestSessionSeekableVP9Opus(t *testing.T) {
	var buf seekBuf
	cfg := DefaultConfig()
	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}

	vnum, err := s.AddTrack(Track{Codec: core.CodecVP9, IsVideo: true, Width: 320, Height: 240})
	if err != nil {
		t.Fatal(err)
	}
	anum, err := s.AddTrack(Track{Codec: core.CodecOpus, Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}

	feed := func(track, dms int, key bool, codec core.CodecType) {
		p := &core.Packet{
			TrackID:    track,
			DTS:        time.Duration(dms) * time.Millisecond,
			PTS:        time.Duration(dms) * time.Millisecond,
			IsKeyframe: key,
			Codec:      codec,
			Data:       []byte{0xAA, 0xBB},
		}
		if err := s.WritePacket(p); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	feed(vnum, 0, true, core.CodecVP9)
	feed(anum, 0, false, core.CodecOpus)
	feed(vnum, 33, false, core.CodecVP9)
	feed(anum, 20, false, core.CodecOpus)
	feed(vnum, 66, false, core.CodecVP9)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatal("no output written")
	}
	if !bytes.Contains(out, []byte("webm")) {
		t.Error("missing webm doctype")
	}
	if !bytes.Contains(out, []byte("V_VP9")) {
		t.Error("missing VP9 codec id")
	}
	if !bytes.Contains(out, []byte("A_OPUS")) {
		t.Error("missing Opus codec id")
	}
	if !bytes.Contains(out, []byte{0x1F, 0x43, 0xB6, 0x75}) {
		t.Error("missing Cluster element")
	}
	if !bytes.Contains(out, []byte{0xA3}) {
		t.Error("missing SimpleBlock")
	}
}

func TestSessionStreamingNonSeekable(t *testing.T) {
	var buf bytes.Buffer
	s, _ := NewSession(&buf, DefaultConfig())
	s.AddTrack(Track{Codec: core.CodecVP9, IsVideo: true, Width: 160, Height: 90})

	p := &core.Packet{
		TrackID: 1, DTS: 0, PTS: 0, IsKeyframe: true,
		Codec: core.CodecVP9, Data: []byte{0x01},
	}
	if err := s.WritePacket(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatal("no output")
	}
	if !bytes.Contains(out, []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Error("streaming mode should use Segment unknown-size sentinel")
	}
}

func TestSessionUnknownTrack(t *testing.T) {
	var buf bytes.Buffer
	s, _ := NewSession(&buf, DefaultConfig())
	p := &core.Packet{TrackID: 99, Data: []byte{0x00}}
	if err := s.WritePacket(p); err == nil {
		t.Error("expected error for unknown track")
	}
}

func TestSessionCloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	s, _ := NewSession(&buf, DefaultConfig())
	s.AddTrack(Track{Codec: core.CodecVP9, IsVideo: true, Width: 16, Height: 16})
	s.WritePacket(&core.Packet{TrackID: 1, DTS: 0, PTS: 0, IsKeyframe: true, Codec: core.CodecVP9, Data: []byte{0x00}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("double close should be safe, got %v", err)
	}
}
