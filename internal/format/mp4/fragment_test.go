package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFragmentedMP4InitTfhdTfdtTrun(t *testing.T) {
	init := fragmentedOpusInit()
	media := fragmentedOpusMedia()
	r, err := NewFragmentReader(bytes.NewReader(init), bytes.NewReader(media))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		dts  int64
		data []byte
	}{{0, []byte{0xf8, 1}}, {960, []byte{0xf8, 2}}}
	for i, expected := range want {
		sample, err := r.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if sample.DTS != expected.dts || sample.PTS != expected.dts || sample.Duration != 960 || sample.Timescale != 48_000 || !bytes.Equal(sample.Data, expected.data) {
			t.Fatalf("sample %d = %+v", i, sample)
		}
	}
	if _, err := r.NextSample(); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v", err)
	}
	actual, err := r.SeekNS(1, 21_000_000)
	if err != nil || actual != 20_000_000 {
		t.Fatalf("seek = %d, %v", actual, err)
	}
	if sample, err := r.NextSample(); err != nil || sample.DTS != 960 {
		t.Fatalf("sample after seek = %+v, %v", sample, err)
	}

	combined := append(append([]byte(nil), init...), media...)
	combinedReader, err := NewReader(bytes.NewReader(combined))
	if err != nil {
		t.Fatal(err)
	}
	if sample, err := combinedReader.NextSample(); err != nil || sample.DTS != 0 || !bytes.Equal(sample.Data, want[0].data) {
		t.Fatalf("combined sample = %+v, %v", sample, err)
	}
}

func TestFragmentedMP4TruncatedRun(t *testing.T) {
	init := fragmentedOpusInit()
	media := fragmentedOpusMedia()
	// Remove one byte from mdat: the top-level declared size must be rejected
	// during bounded parsing, before any sample offset can escape the source.
	if _, err := NewFragmentReader(bytes.NewReader(init), bytes.NewReader(media[:len(media)-1])); err == nil || (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
		t.Fatalf("truncated fragment = %v", err)
	}
}

func fragmentedOpusInit() []byte {
	dops := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 0}
	audio := make([]byte, 28)
	binary.BigEndian.PutUint16(audio[6:8], 1)
	binary.BigEndian.PutUint16(audio[16:18], 2)
	binary.BigEndian.PutUint32(audio[24:28], 48_000<<16)
	audio = append(audio, mkBox("dOps", dops)...)
	entry := make([]byte, 8)
	binary.BigEndian.PutUint32(entry, uint32(8+len(audio)))
	copy(entry[4:8], "Opus")
	entry = append(entry, audio...)
	stsdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(stsdPayload, 1)
	stsdPayload = append(stsdPayload, entry...)
	stbl := mkBox("stbl", bytes.Join([][]byte{
		fullBox("stsd", stsdPayload),
		fullBox("stts", []byte{0, 0, 0, 0}),
		fullBox("stsz", make([]byte, 8)),
	}, nil))
	mdhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhd[8:12], 48_000)
	tkhd := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhd[8:12], 1)
	trak := mkBox("trak", append(fullBox("tkhd", tkhd), mkBox("mdia", append(fullBox("mdhd", mdhd), mkBox("minf", stbl)...))...))
	trex := make([]byte, 20)
	binary.BigEndian.PutUint32(trex[0:4], 1)
	binary.BigEndian.PutUint32(trex[4:8], 1)
	binary.BigEndian.PutUint32(trex[8:12], 960)
	binary.BigEndian.PutUint32(trex[12:16], 2)
	mvex := mkBox("mvex", fullBox("trex", trex))
	mvhd := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhd[8:12], 1000)
	return append(mkBox("ftyp", ftypPayload()), mkBox("moov", bytes.Join([][]byte{fullBox("mvhd", mvhd), trak, mvex}, nil))...)
}

func fragmentedOpusMedia() []byte {
	buildMoof := func(dataOffset int32) []byte {
		// Construct full-box payload explicitly: flags are bytes 1..3.
		tfhd := []byte{0, 0x02, 0, 0, 0, 0, 0, 1}
		tfdt := make([]byte, 12)
		tfdt[0] = 1
		trun := make([]byte, 12)
		trun[3] = 0x01 // data-offset-present
		binary.BigEndian.PutUint32(trun[4:8], 2)
		binary.BigEndian.PutUint32(trun[8:12], uint32(dataOffset))
		traf := mkBox("traf", bytes.Join([][]byte{mkBox("tfhd", tfhd), mkBox("tfdt", tfdt), mkBox("trun", trun)}, nil))
		return mkBox("moof", append(fullBox("mfhd", []byte{0, 0, 0, 1}), traf...))
	}
	moof := buildMoof(0)
	moof = buildMoof(int32(len(moof) + 8))
	return append(moof, mkBox("mdat", []byte{0xf8, 1, 0xf8, 2})...)
}
