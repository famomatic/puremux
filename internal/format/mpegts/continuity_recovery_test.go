package mpegts

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

type interruptedTS struct {
	*bytes.Reader
	at     int64
	failed bool
}

func (r *interruptedTS) Read(p []byte) (int, error) {
	pos, _ := r.Reader.Seek(0, io.SeekCurrent)
	if !r.failed {
		if pos == r.at {
			r.failed = true
			return 0, context.DeadlineExceeded
		}
		if pos < r.at {
			p = p[:min(int64(len(p)), r.at-pos)]
		}
	}
	return r.Reader.Read(p)
}
func TestStreamingTSResumesPartialPacket(t *testing.T) {
	data := audit20260906TS(t)
	next := append([]byte(nil), data[len(data)-188:]...)
	next[3] = next[3]&0xf0 | 1
	data = append(data, next...)
	r, err := NewStreamingInputReader(&interruptedTS{Reader: bytes.NewReader(data), at: int64(len(data) - 188 + 40)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.NextPacket(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.NextPacket(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p, err := r.NextPacket()
	if err != nil || !bytes.Equal(p.Data, []byte{1}) {
		t.Fatalf("resume %+v %v", p, err)
	}
}
func TestPSIAssemblyMalformedBoundaries(t *testing.T) {
	for _, data := range [][]byte{nil, {1}, {0, 0, 0xbf, 0xff}, {0, 0, 0x30, 9}} {
		s := &StreamingInputReader{}
		if _, err := s.assemblePSI(0, data, true); err == nil {
			t.Fatalf("malformed PSI accepted %x", data)
		}
	}
	s := &StreamingInputReader{}
	if _, err := s.assemblePSI(0, []byte{0, 0, 0xb0}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assemblePSI(0, []byte{0, 0xff}, true); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unfinished pointer=%v", err)
	}
}
func TestTSTripleDuplicateRejected(t *testing.T) {
	data := audit20260906TS(t)
	triple := append(append(append([]byte(nil), data[:188]...), data[:188]...), data...)
	if _, err := NewInputReader(bytes.NewReader(triple)); err == nil {
		t.Fatal("third duplicate accepted")
	}
}

func TestPTSAndDTSInitialWrapAlignment(t *testing.T) {
	mod := int64(1) << 33
	payload := []byte{0, 0, 1, 0x65, 1}
	data := append(pesHeader(0xe0, 2, uint64(mod-2), len(payload)), payload...)
	packets, _, err := parsePESWithClock(&pesBuffer{data: data}, 0, core.CodecH264, &pesClock{})
	if err != nil || packets[0].PTS != mod+2 || packets[0].DTS != mod-2 {
		t.Fatalf("packets=%+v err=%v", packets, err)
	}
	var epoch timestampEpoch
	for i, v := range []int64{0, mod - 1, 1} {
		want := []int64{0, -1, 1}[i]
		got, err := epoch.unwrap(v)
		if err != nil || got != want {
			t.Fatalf("got %d want %d: %v", got, want, err)
		}
	}
}

func FuzzStreamingTSBoundaries(f *testing.F) {
	null := bytes.Repeat([]byte{0xff}, 188)
	copy(null, []byte{0x47, 0x1f, 0xff, 0x10})
	f.Add(null)
	f.Add([]byte{0x47, 0x40, 0, 0x10, 0, 0, 0xb0, 0xd1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_, _ = NewInputReader(bytes.NewReader(data))
	})
}

func TestPMTConfigurationChangeRejected(t *testing.T) {
	data := audit20260906TS(t)
	for _, change := range []bool{false, true} {
		pmt := append([]byte(nil), data[188:376]...)
		pmt[3] = pmt[3]&0xf0 | 1
		offset := 4
		if pmt[3]&0x20 != 0 {
			offset = 5 + int(pmt[4])
		}
		offset += 1 + int(pmt[offset])
		length := 3 + (int(pmt[offset+1]&15)<<8 | int(pmt[offset+2]))
		section := pmt[offset : offset+length]
		section[5] += 2 // version increment, current_next remains one
		if change {
			section[12] = 0x03
		} // AAC stream_type 0F -> MPEG audio 03
		crc := crc32MPEG(section[:len(section)-4])
		binary.BigEndian.PutUint32(section[len(section)-4:], crc)
		_, err := NewInputReader(bytes.NewReader(append(append([]byte(nil), data...), pmt...)))
		if change && (err == nil || !strings.Contains(err.Error(), "PMT stream change")) {
			t.Fatalf("change=%v", err)
		}
		if !change && err != nil {
			t.Fatalf("version-only update=%v", err)
		}
	}
}
