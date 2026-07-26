package puremux

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// --- minimal TS parse-back (independent of internal/format/mpegts) ----------

type pesInfo struct {
	pts    uint64
	dts    uint64 // == pts when the PES header carried no separate DTS
	hasDTS bool   // PTS_DTS_flags was 0b11
	es     []byte
}

// pes33 decodes the 5-byte 33-bit PES timestamp field.
func pes33(f []byte) uint64 {
	return uint64(f[0]&0x0E)<<29 | uint64(f[1])<<22 |
		uint64(f[2]&0xFE)<<14 | uint64(f[3])<<7 | uint64(f[4])>>1
}

// demuxTS extracts completed PES packets for one PID from a transport stream.
func demuxTS(t *testing.T, raw []byte, pid uint16) []pesInfo {
	t.Helper()
	if len(raw)%188 != 0 {
		t.Fatalf("not 188-aligned: %d", len(raw))
	}
	var out []pesInfo
	var cur []byte
	flush := func() {
		if cur == nil {
			return
		}
		if len(cur) < 9 || cur[0] != 0 || cur[1] != 0 || cur[2] != 1 {
			t.Fatalf("bad PES header: % X", cur[:min(9, len(cur))])
		}
		hdrLen := int(cur[8])
		var pts, dts uint64
		hasDTS := false
		if cur[7]&0x80 != 0 {
			pts = pes33(cur[9:14])
			dts = pts
		}
		if cur[7]&0xC0 == 0xC0 {
			dts = pes33(cur[14:19])
			hasDTS = true
		}
		out = append(out, pesInfo{pts: pts, dts: dts, hasDTS: hasDTS, es: append([]byte{}, cur[9+hdrLen:]...)})
		cur = nil
	}
	for off := 0; off < len(raw); off += 188 {
		pkt := raw[off : off+188]
		if pkt[0] != 0x47 {
			t.Fatalf("sync lost at %d", off)
		}
		p := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if p != pid || pkt[3]&0x10 == 0 {
			continue
		}
		payload := pkt[4:]
		if pkt[3]&0x20 != 0 {
			payload = payload[1+int(payload[0]):]
		}
		if pkt[1]&0x40 != 0 {
			flush()
			cur = append([]byte{}, payload...)
		} else if cur != nil {
			cur = append(cur, payload...)
		}
	}
	flush()
	return out
}

// annexBAU builds a start-code framed AU with the given NAL header byte.
func annexBAU(nalHeader byte, fill byte) []byte {
	return []byte{0x00, 0x00, 0x00, 0x01, nalHeader, fill, fill, fill}
}

// adts48k builds one spec-accurate 48 kHz stereo AAC-LC ADTS frame.
func adts48k(payload []byte) []byte {
	frameLen := 7 + len(payload)
	h := []byte{0xFF, 0xF1, 0x4C,
		0x80 | byte(frameLen>>11), byte(frameLen >> 3),
		byte(frameLen&0x07)<<5 | 0x1F, 0xFC}
	return append(h, payload...)
}

// --- end-to-end: live TS session absorbs broken source timestamps -----------

func TestSessionMPEGTSLiveTimestampRepair(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMPEGTS
	cfg.Preprocessor.MaxBufferSize = 32
	cfg.Preprocessor.MaxBufferDuration = 50_000_000            // 50ms reorder window
	cfg.Preprocessor.MinMonotonicStep = uint64(time.Millisecond) // duplicates -> +1ms

	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true, Width: 1920, Height: 1080})
	if err != nil {
		t.Fatal(err)
	}
	aud, err := s.AddTrack(Track{Codec: CodecAAC, Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}

	idr := annexBAU(0x65, 0xA0)    // IDR slice -> keyframe
	nonIDR := annexBAU(0x41, 0xB0) // non-IDR slice

	// Simulates the SOOP live pathology:
	//  - a pre-keyframe frame (must be dropped by the Aligner),
	//  - a keyframe, normal frames,
	//  - a duplicate-timestamp burst (3 frames at 160ms),
	//  - an out-of-order pair (200ms arrives before 180ms).
	if err := s.WriteVideo(vid, nonIDR, 80*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteVideo(vid, idr, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for _, ms := range []int{120, 140, 160, 160, 160, 200, 180, 260} {
		if err := s.WriteVideo(vid, nonIDR, time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	// Two concatenated ADTS frames in one chunk stamped 110ms; the second
	// must advance by 1024/48000 s.
	chunk := append(adts48k([]byte{0x01, 0x02}), adts48k([]byte{0x03, 0x04})...)
	if err := s.WriteADTS(aud, chunk, 110*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	video := demuxTS(t, buf.Bytes(), 0x100)
	// 10 fed video AUs, 1 dropped pre-keyframe -> 9 PES packets.
	if len(video) != 9 {
		t.Fatalf("video PES count = %d, want 9", len(video))
	}
	// First emitted video AU must be the keyframe (pre-keyframe frame gone).
	if !bytes.Equal(video[0].es, idr) {
		t.Fatalf("first video AU is not the IDR: % X", video[0].es)
	}
	// PES PTS must be STRICTLY increasing at 90 kHz granularity — this is the
	// property whose absence produced "mpegts: DTS out of order" and mpv's
	// "Invalid video timestamp" on the raw source stream.
	for i := 1; i < len(video); i++ {
		if video[i].pts <= video[i-1].pts {
			t.Fatalf("video PTS not strictly increasing at %d: %d <= %d",
				i, video[i].pts, video[i-1].pts)
		}
	}
	// The reordered pair must come out sorted: the AU stamped 180ms before
	// the one stamped 200ms. Emission order is
	// [100,120,140,160,161,162,180,200,260] (ms), so indexes 6/7 are the
	// pair and their PTS delta is 20ms = 1800 ticks.
	if d := video[7].pts - video[6].pts; d != 1800 {
		t.Fatalf("reordered pair delta = %d ticks, want 1800", d)
	}

	audio := demuxTS(t, buf.Bytes(), 0x101)
	if len(audio) != 2 {
		t.Fatalf("audio PES count = %d, want 2", len(audio))
	}
	// 1024 samples @48kHz = 21.333ms = 1920 ticks exactly.
	if d := audio[1].pts - audio[0].pts; d != 1920 {
		t.Fatalf("ADTS frame spacing = %d ticks, want 1920", d)
	}
}

func TestSessionMPEGTSRejectsWebMCodecs(t *testing.T) {
	s, err := NewSession(&bytes.Buffer{}, Config{OutputContainer: ContainerMPEGTS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTrack(Track{Codec: CodecVP9, IsVideo: true}); err == nil {
		t.Fatal("VP9 must be rejected in MPEG-TS output")
	}
	if _, err := s.AddTrack(Track{Codec: CodecOpus}); err == nil {
		t.Fatal("Opus must be rejected in MPEG-TS output")
	}
}

func TestMPEGTSCapability(t *testing.T) {
	found := false
	for _, o := range SupportedOutputs() {
		if o.Container == ContainerMPEGTS {
			found = o.CanWrite
		}
	}
	if !found {
		t.Fatal("ContainerMPEGTS missing from SupportedOutputs")
	}
	if !CanRemuxCodecs(ContainerMPEGTS, []CodecType{CodecH264, CodecAAC}) {
		t.Fatal("H264+AAC must be expressible in MPEG-TS")
	}
	if CanRemuxCodecs(ContainerMPEGTS, []CodecType{CodecVP9}) {
		t.Fatal("VP9 must not be expressible in MPEG-TS")
	}
	if ContainerMPEGTS.String() != "mpegts" || ContainerMPEGTS.Extension() != "ts" {
		t.Fatalf("naming: %q / %q", ContainerMPEGTS.String(), ContainerMPEGTS.Extension())
	}
}

func TestMergeRejectsMPEGTSOutput(t *testing.T) {
	// File-based Merge must refuse .ts output (Session live API only): file
	// inputs carry AVCC/raw-AAC framing that a TS cannot hold verbatim.
	err := Merge(context.Background(), []string{"in.webm"},
		filepath.Join(t.TempDir(), "out.ts"), DefaultConfig())
	if !errors.Is(err, ErrUnsupportedOutput) {
		t.Fatalf("Merge to .ts: err = %v, want ErrUnsupportedOutput", err)
	}
	err = MergeToWriter(context.Background(), []string{"in.webm"},
		&bytes.Buffer{}, ContainerMPEGTS, DefaultConfig())
	if !errors.Is(err, ErrUnsupportedOutput) {
		t.Fatalf("MergeToWriter TS: err = %v, want ErrUnsupportedOutput", err)
	}
}

func TestSessionMPEGTSKeyframeFirstDuplicateBurst(t *testing.T) {
	// The SOOP startup shape: the keyframe AND its following frames all share
	// one backfill timestamp. Regression for the Enforcer's unstable
	// equal-DTS insertion, which reversed the burst so the Aligner dropped
	// every frame and the stream started 9 frames late.
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMPEGTS
	cfg.Preprocessor.MinMonotonicStep = uint64(time.Millisecond)
	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vid, err := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	if err != nil {
		t.Fatal(err)
	}

	idr := annexBAU(0x65, 0x00)
	burst := [][]byte{idr}
	for i := byte(1); i < 5; i++ {
		burst = append(burst, annexBAU(0x41, i)) // distinct payloads
	}
	t0 := 500 * time.Millisecond
	for _, au := range burst {
		if err := s.WriteVideo(vid, au, t0); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		if err := s.WriteVideo(vid, annexBAU(0x41, 0xF0+byte(i)), t0+time.Duration(i)*20*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	video := demuxTS(t, buf.Bytes(), 0x100)
	// Nothing may be dropped: 5 burst + 3 normal = 8 PES.
	if len(video) != 8 {
		t.Fatalf("video PES count = %d, want 8 (burst frames dropped?)", len(video))
	}
	// Arrival order preserved: IDR first, then F1..F4.
	if !bytes.Equal(video[0].es, idr) {
		t.Fatalf("first PES is not the IDR: % X", video[0].es)
	}
	for i := 1; i < 5; i++ {
		if video[i].es[5] != byte(i) {
			t.Fatalf("burst frame %d out of order (payload marker %#x)", i, video[i].es[5])
		}
	}
	// First PES sits at the rebase origin (the burst was not shifted onto a
	// nudged base), and PTS is strictly increasing throughout.
	if video[0].pts != 900000 {
		t.Fatalf("first PES PTS = %d, want 900000", video[0].pts)
	}
	for i := 1; i < len(video); i++ {
		if video[i].pts <= video[i-1].pts {
			t.Fatalf("PTS not strictly increasing at %d", i)
		}
	}
}

func TestLiveWriteCopiesCallerBuffer(t *testing.T) {
	// API contract pinned by test: WriteVideo/WriteADTS copy the payload
	// before it enters the (packet-holding) pipeline, so callers may reuse
	// their buffer immediately — ytv1's SOOP deframer reuses its carry
	// buffer between pushes and relies on this. A zero-copy change to the
	// live helpers must fail here.
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMPEGTS
	s, err := NewSession(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vid, _ := s.AddTrack(Track{Codec: CodecH264, IsVideo: true})
	aud, _ := s.AddTrack(Track{Codec: CodecAAC})

	shared := make([]byte, 0, 256)
	idr := annexBAU(0x65, 0xAA)
	shared = append(shared[:0], idr...)
	if err := s.WriteVideo(vid, shared, 0); err != nil {
		t.Fatal(err)
	}
	// Clobber the caller buffer, as the deframer's next push would.
	for i := range shared {
		shared[i] = 0xEE
	}

	adts := adts48k([]byte{0x42, 0x43})
	shared = append(shared[:0], adts...)
	if err := s.WriteADTS(aud, shared, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for i := range shared {
		shared[i] = 0xEE
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	video := demuxTS(t, buf.Bytes(), 0x100)
	if len(video) != 1 || !bytes.Equal(video[0].es, idr) {
		t.Fatalf("video payload corrupted by caller buffer reuse")
	}
	audio := demuxTS(t, buf.Bytes(), 0x101)
	if len(audio) != 1 || !bytes.Equal(audio[0].es, adts) {
		t.Fatalf("audio payload corrupted by caller buffer reuse")
	}
}
