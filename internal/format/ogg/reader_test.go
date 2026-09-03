package ogg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestOfficialXiphOpusHeaderPage(t *testing.T) {
	encoded, err := os.ReadFile("testdata/xiph_speech_music_headers.opus.b64")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := readPage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if p.flags != 0x02 || p.serial != 0x5e21bdb3 || p.sequence != 0 || p.granule != 0 || len(p.body) != 19 {
		t.Fatalf("unexpected official page: %+v", p)
	}
	head, err := parseOpusHead(p.body)
	if err != nil {
		t.Fatal(err)
	}
	if head.Version != 1 || head.Channels != 2 || head.PreSkip != 312 || head.InputSampleRate != 48_000 || head.MappingFamily != 0 {
		t.Fatalf("unexpected OpusHead: %+v", head)
	}

	corrupt := append([]byte(nil), data...)
	corrupt[28] ^= 1
	if _, err := readPage(bytes.NewReader(corrupt), int64(len(corrupt))); err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("corrupt official page = %v", err)
	}
}

func TestOggOpusPacketsTimingAndSeek(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	tags := opusTags("test-vendor", "TITLE=spec packet")
	// RFC 6716 TOC 0xF8 is config 31, code 0: one 20 ms CELT frame.
	audio1 := []byte{0xf8, 0x11}
	audio2 := []byte{0xf8, 0x22}
	stream := append(makePage(0x02, 0, 7, 0, head), makePage(0, 0, 7, 1, tags)...)
	stream = append(stream, makePage(0x04, 312+2*960, 7, 2, audio1, audio2)...)
	r, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.DurationSamples(); got != 1920 {
		t.Fatalf("duration = %d", got)
	}
	for i, wantPTS := range []int64{0, 960} {
		packet, err := r.NextPacket(context.Background())
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if packet.PTS != wantPTS || packet.Duration != 960 || packet.Data[1] != byte(0x11+i*0x11) {
			t.Fatalf("packet %d = %+v", i, packet)
		}
	}
	if _, err := r.NextPacket(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v", err)
	}
	actual, err := r.SeekSamples(context.Background(), 1000)
	if err != nil || actual != 0 {
		t.Fatalf("seek = %d, %v", actual, err)
	}
	if packet, err := r.NextPacket(context.Background()); err != nil || packet.PTS != 0 {
		t.Fatalf("packet after seek = %+v, %v", packet, err)
	}
}

func TestOggBoundaries(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("OggS"), makePage(0x02, 0, 1, 0, []byte("not opus"))} {
		if _, err := NewReader(bytes.NewReader(data)); err == nil {
			t.Fatalf("malformed input accepted: %x", data)
		}
	}
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	badContinuation := append(makePage(0x02, 0, 1, 0, head), makePage(0x01, 0, 1, 1, opusTags("v"))...)
	if _, err := NewReader(bytes.NewReader(badContinuation)); err == nil {
		t.Fatal("continued flag without partial packet accepted")
	}
	if _, err := parseOpusHead([]byte("OpusHead")); !errors.Is(err, io.ErrUnexpectedEOF) && err == nil {
		t.Fatalf("truncated OpusHead = %v", err)
	}
	if _, err := parseOpusTags(append([]byte("OpusTags"), 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("oversized OpusTags = %v", err)
	}
}

func makePage(flags byte, granule uint64, serial, sequence uint32, packets ...[]byte) []byte {
	var lacing []byte
	var body []byte
	for _, packet := range packets {
		remaining := len(packet)
		for remaining >= 255 {
			lacing = append(lacing, 255)
			body = append(body, packet[len(packet)-remaining:len(packet)-remaining+255]...)
			remaining -= 255
		}
		lacing = append(lacing, byte(remaining))
		body = append(body, packet[len(packet)-remaining:]...)
	}
	page := make([]byte, 27+len(lacing)+len(body))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	copy(page[27+len(lacing):], body)
	binary.LittleEndian.PutUint32(page[22:26], CRC(page))
	return page
}

func opusTags(vendor string, comments ...string) []byte {
	var buf bytes.Buffer
	buf.WriteString("OpusTags")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(vendor)))
	buf.WriteString(vendor)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(comments)))
	for _, comment := range comments {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(comment)))
		buf.WriteString(comment)
	}
	return buf.Bytes()
}
