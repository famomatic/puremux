package mpegts

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// --- spec-derived primitives -------------------------------------------------

func TestCRC32MPEG2CheckValue(t *testing.T) {
	// Published CRC-32/MPEG-2 catalogue check value: "123456789" -> 0x0376E6E7.
	if got := crc32MPEG([]byte("123456789")); got != 0x0376E6E7 {
		t.Fatalf("crc32MPEG = %#08x, want 0x0376E6E7", got)
	}
}

func TestPATSectionBytes(t *testing.T) {
	// Hand-derived PAT: table_id 0x00, section_length 13 (5 fixed + 4 program
	// + 4 CRC), TSID 1, version 0/current 1 (0xC1), program 1 -> PMT PID
	// 0x1000 (reserved bits 0xE0 | 0x10 = 0xF0).
	want := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, 0xF0, 0x00}
	got := buildPAT()
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("PAT = % X, want prefix % X", got, want)
	}
	if len(got) != len(want)+4 {
		t.Fatalf("PAT length %d, want %d", len(got), len(want)+4)
	}
}

func TestPMTSectionBytes(t *testing.T) {
	m := New(&bytes.Buffer{})
	if _, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(core.Track{ID: 2, Kind: core.TrackAudio, Codec: core.CodecAAC}); err != nil {
		t.Fatal(err)
	}
	// Hand-derived: table_id 0x02, section_length 9+5*2+4 = 23 (0x17),
	// program 1, PCR_PID = video PID 0x100 (0xE0|0x01, 0x00), empty program
	// info, then [stream_type, PID, ES info len 0] per track:
	// H.264 = 0x1B @ 0x100, ADTS AAC = 0x0F @ 0x101.
	want := []byte{0x02, 0xB0, 0x17, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0xE1, 0x00, 0xF0, 0x00,
		0x1B, 0xE1, 0x00, 0xF0, 0x00,
		0x0F, 0xE1, 0x01, 0xF0, 0x00}
	got := m.buildPMT()
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("PMT = % X\nwant prefix % X", got, want)
	}
}

func TestPESTimestampEncoding(t *testing.T) {
	// PTS = 900000 (0xDBBA0). Hand-derived 5-byte field with '0010' prefix:
	//   PTS[32..30]=0            -> byte0 = 0x21
	//   PTS[29..22]=0            -> byte1 = 0x00
	//   PTS[21..15]=27 (0x1B)    -> byte2 = 0x1B<<1|1 = 0x37
	//   PTS[14..7] =0x77         -> byte3 = 0x77
	//   PTS[6..0]  =32           -> byte4 = 32<<1|1 = 0x41
	got := appendTimestamp(nil, 0x20, 900000)
	want := []byte{0x21, 0x00, 0x37, 0x77, 0x41}
	if !bytes.Equal(got, want) {
		t.Fatalf("PTS field = % X, want % X", got, want)
	}
	// All-ones 33-bit value: markers must survive.
	got = appendTimestamp(nil, 0x20, ptsMask)
	want = []byte{0x2F, 0xFF, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(got, want) {
		t.Fatalf("max PTS field = % X, want % X", got, want)
	}
}

func TestPESHeaderPTSOnlyVsPTSDTS(t *testing.T) {
	// pts == dts -> flags 0x80 (PTS only), header data length 5.
	h := pesHeader(0xE0, 900000, 900000, 4)
	if h[3] != 0xE0 || h[6] != 0x80 || h[7] != 0x80 || h[8] != 0x05 {
		t.Fatalf("PTS-only header = % X", h[:9])
	}
	if wantLen := 3 + 5 + 4; int(h[4])<<8|int(h[5]) != wantLen {
		t.Fatalf("PES_packet_length = %d, want %d", int(h[4])<<8|int(h[5]), wantLen)
	}
	// pts != dts -> flags 0xC0, header data length 10, prefixes 0x3 and 0x1.
	h = pesHeader(0xE0, 903600, 900000, 4)
	if h[7] != 0xC0 || h[8] != 0x0A {
		t.Fatalf("PTS+DTS header flags = % X", h[6:9])
	}
	if h[9]&0xF0 != 0x30 {
		t.Fatalf("PTS guard nibble = %#x, want 0x3X", h[9])
	}
	if h[14]&0xF0 != 0x10 {
		t.Fatalf("DTS guard nibble = %#x, want 0x1X", h[14])
	}
}

func TestPESLengthUnboundedForHugePayload(t *testing.T) {
	h := pesHeader(0xE0, 0, 0, 0x10000)
	if h[4] != 0 || h[5] != 0 {
		t.Fatalf("huge payload must use unbounded PES length, got %#x %#x", h[4], h[5])
	}
}

// --- TS-level parse-back helpers --------------------------------------------

// tsPacket is one parsed 188-byte packet.
type tsPacket struct {
	pid     uint16
	pusi    bool
	payload []byte
}

func parseTS(t *testing.T, b []byte) []tsPacket {
	t.Helper()
	if len(b)%pktLen != 0 {
		t.Fatalf("output not 188-aligned: %d bytes", len(b))
	}
	var out []tsPacket
	for off := 0; off < len(b); off += pktLen {
		pkt := b[off : off+pktLen]
		if pkt[0] != syncByte {
			t.Fatalf("packet at %d lacks sync byte: %#x", off, pkt[0])
		}
		p := tsPacket{
			pid:  uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2]),
			pusi: pkt[1]&0x40 != 0,
		}
		payload := pkt[4:]
		if pkt[3]&0x20 != 0 { // adaptation field present
			afLen := int(payload[0])
			payload = payload[1+afLen:]
		}
		if pkt[3]&0x10 != 0 {
			p.payload = payload
		}
		out = append(out, p)
	}
	return out
}

// collectPES reassembles PES payloads for a PID, returning (pts values, ES data).
func collectPES(t *testing.T, pkts []tsPacket, pid uint16) ([]uint64, []byte) {
	t.Helper()
	var ptss []uint64
	var es []byte
	var cur []byte
	flush := func() {
		if cur == nil {
			return
		}
		if len(cur) < 9 || cur[0] != 0 || cur[1] != 0 || cur[2] != 1 {
			t.Fatalf("bad PES start: % X", cur[:min(9, len(cur))])
		}
		hdrLen := int(cur[8])
		flags := cur[7]
		if flags&0x80 != 0 {
			f := cur[9:14]
			pts := uint64(f[0]&0x0E)<<29 | uint64(f[1])<<22 |
				uint64(f[2]&0xFE)<<14 | uint64(f[3])<<7 | uint64(f[4])>>1
			ptss = append(ptss, pts)
		}
		es = append(es, cur[9+hdrLen:]...)
		cur = nil
	}
	for _, p := range pkts {
		if p.pid != pid || p.payload == nil {
			continue
		}
		if p.pusi {
			flush()
			cur = append([]byte{}, p.payload...)
		} else if cur != nil {
			cur = append(cur, p.payload...)
		}
	}
	flush()
	return ptss, es
}

// --- end-to-end muxer behavior ----------------------------------------------

func TestMuxerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	vid, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	if err != nil {
		t.Fatal(err)
	}
	aud, err := m.AddTrack(core.Track{ID: 2, Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}

	// Large AU spanning multiple TS packets + a small audio frame.
	au := make([]byte, 500)
	for i := range au {
		au[i] = byte(i)
	}
	base := 5 * time.Second
	if err := m.WritePacket(&core.Packet{TrackID: vid, Codec: core.CodecH264,
		PTS: base, DTS: base, Data: au}); err != nil {
		t.Fatal(err)
	}
	adts := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x7F, 0xFC, 0xAA, 0xBB, 0xCC, 0xDD}
	if err := m.WritePacket(&core.Packet{TrackID: aud, Codec: core.CodecAAC,
		PTS: base + 10*time.Millisecond, DTS: base + 10*time.Millisecond, Data: adts}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	pkts := parseTS(t, buf.Bytes())
	// First two packets must be PAT then PMT.
	if pkts[0].pid != pidPAT || pkts[1].pid != pidPMT {
		t.Fatalf("stream must open with PAT+PMT, got PIDs %#x %#x", pkts[0].pid, pkts[1].pid)
	}

	vPTS, vES := collectPES(t, pkts, 0x100)
	if !bytes.Equal(vES, au) {
		t.Fatalf("video ES corrupted: %d bytes vs %d", len(vES), len(au))
	}
	// First packet is rebased to the 10s headroom offset: 900000 ticks.
	if len(vPTS) != 1 || vPTS[0] != 900000 {
		t.Fatalf("video PTS = %v, want [900000]", vPTS)
	}

	aPTS, aES := collectPES(t, pkts, 0x101)
	if !bytes.Equal(aES, adts) {
		t.Fatalf("audio ES corrupted")
	}
	// +10ms = +900 ticks.
	if len(aPTS) != 1 || aPTS[0] != 900900 {
		t.Fatalf("audio PTS = %v, want [900900]", aPTS)
	}
}

func TestMuxerPCROnVideoPIDOnly(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	vid, _ := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	aud, _ := m.AddTrack(core.Track{ID: 2, Kind: core.TrackAudio, Codec: core.CodecAAC})
	_ = m.WritePacket(&core.Packet{TrackID: vid, PTS: 0, DTS: 0, Data: []byte{1}})
	_ = m.WritePacket(&core.Packet{TrackID: aud, PTS: 0, DTS: 0, Data: []byte{2}})

	raw := buf.Bytes()
	sawVideoPCR := false
	for off := 0; off < len(raw); off += pktLen {
		pkt := raw[off : off+pktLen]
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pkt[3]&0x20 == 0 {
			continue
		}
		afLen := int(pkt[4])
		if afLen > 0 && pkt[5]&0x10 != 0 { // PCR_flag
			if pid != 0x100 {
				t.Fatalf("PCR on PID %#x, want video 0x100 only", pid)
			}
			sawVideoPCR = true
		}
	}
	if !sawVideoPCR {
		t.Fatal("no PCR found on video PID")
	}
}

func TestMuxerContinuityCounters(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	vid, _ := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	big := make([]byte, 3*184)
	for i := range 3 {
		_ = m.WritePacket(&core.Packet{TrackID: vid,
			PTS: time.Duration(i) * time.Second, DTS: time.Duration(i) * time.Second, Data: big})
	}
	raw := buf.Bytes()
	last := -1
	for off := 0; off < len(raw); off += pktLen {
		pkt := raw[off : off+pktLen]
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pid != 0x100 {
			continue
		}
		cc := int(pkt[3] & 0x0F)
		if last >= 0 && cc != (last+1)&0x0F {
			t.Fatalf("continuity jump: %d -> %d", last, cc)
		}
		last = cc
	}
	if last < 0 {
		t.Fatal("no video packets seen")
	}
}

func TestMuxerTablesReemitted(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	vid, _ := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	for i := range tablesInterval + 2 {
		_ = m.WritePacket(&core.Packet{TrackID: vid,
			PTS: time.Duration(i) * 20 * time.Millisecond,
			DTS: time.Duration(i) * 20 * time.Millisecond, Data: []byte{0x00}})
	}
	patCount := 0
	for _, p := range parseTS(t, buf.Bytes()) {
		if p.pid == pidPAT {
			patCount++
		}
	}
	if patCount < 2 {
		t.Fatalf("PAT emitted %d times, want >= 2 (periodic re-emission)", patCount)
	}
}

func TestMuxerTimestampBaseAndClamp(t *testing.T) {
	m := New(&bytes.Buffer{})
	m.haveBase = true
	m.base = 100 * time.Second
	if got := m.toTicks(100 * time.Second); got != ptsOffset {
		t.Fatalf("base ticks = %d, want %d", got, ptsOffset)
	}
	// 5s before base: 10s headroom absorbs it -> 450000.
	if got := m.toTicks(95 * time.Second); got != 450000 {
		t.Fatalf("pre-base ticks = %d, want 450000", got)
	}
	// 20s before base exceeds headroom -> clamped to 0, never negative-wrapped.
	if got := m.toTicks(80 * time.Second); got != 0 {
		t.Fatalf("clamped ticks = %d, want 0", got)
	}
	// 30h past base exceeds the point where int64(rel)*90000 overflowed (~28.5h)
	// — verify the exact masked value, so a silent overflow (which clamps to 0
	// and would still satisfy the <= ptsMask bound) is caught.
	if got := m.toTicks(100*time.Second + 30*time.Hour); got != 1130965408 {
		t.Fatalf("30h ticks = %d, want 1130965408 (no overflow, masked to 33-bit)", got)
	}
}

func TestMuxerErrors(t *testing.T) {
	m := New(&bytes.Buffer{})
	if _, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecVP9}); err != ErrUnsupportedCodec {
		t.Fatalf("VP9 in TS: err = %v", err)
	}
	vid, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackAudio, Codec: core.CodecAAC}); err == nil {
		t.Fatal("duplicate track ID must fail")
	}
	if err := m.WritePacket(&core.Packet{TrackID: 99, Data: []byte{1}}); err != ErrUnknownTrack {
		t.Fatalf("unknown track: err = %v", err)
	}
	if err := m.WritePacket(&core.Packet{TrackID: vid, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(core.Track{ID: 3, Kind: core.TrackAudio, Codec: core.CodecAAC}); err != ErrTrackAfterStart {
		t.Fatalf("AddTrack after start: err = %v", err)
	}
	if err := m.WritePacket(nil); err != nil {
		t.Fatalf("nil packet must be a no-op, got %v", err)
	}
}

func TestMuxerEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	vid, _ := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	if err := m.WritePacket(&core.Packet{TrackID: vid, Data: nil}); err != nil {
		t.Fatal(err)
	}
	// Still a valid 188-aligned stream carrying an empty PES.
	pkts := parseTS(t, buf.Bytes())
	_, es := collectPES(t, pkts, 0x100)
	if len(es) != 0 {
		t.Fatalf("empty AU produced %d ES bytes", len(es))
	}
}

type tsShortWriter struct{}

func (tsShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestMuxerSurfacesShortWrite(t *testing.T) {
	m := New(tsShortWriter{})
	vid, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WritePacket(&core.Packet{TrackID: vid, Data: []byte{1}}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
}

func TestMuxerBoundsPMTTrackCount(t *testing.T) {
	m := New(io.Discard)
	if _, err := m.AddTrack(core.Track{ID: 1, Kind: core.TrackVideo, Codec: core.CodecH264}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		if _, err := m.AddTrack(core.Track{ID: i + 2, Kind: core.TrackAudio, Codec: core.CodecAAC}); err != nil {
			t.Fatalf("track %d: %v", i+2, err)
		}
	}
	if _, err := m.AddTrack(core.Track{ID: 34, Kind: core.TrackVideo, Codec: core.CodecH264}); err == nil {
		t.Fatal("34th track exceeded single-packet PMT capacity")
	}
}
