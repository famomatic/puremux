package puremux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// MP4 box helpers (ISO/IEC 14496-12), spec-derived. These mirror the
// internal mp4 test builders so the end-to-end test stays self-contained in
// the public package without importing internals.
func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func mp4FullBox(typ string, payload []byte) []byte {
	hdr := make([]byte, 4) // version=0, flags=0
	return mp4Box(typ, append(hdr, payload...))
}

func mp4Ftyp() []byte {
	p := make([]byte, 8)
	copy(p[0:4], "isom")
	binary.BigEndian.PutUint32(p[4:8], 0x200)
	p = append(p, "isom"...)
	return mp4Box("ftyp", p)
}

// mp4Mdat returns the mdat box and its payload offset (= box start + 8).
func mp4Mdat(payload []byte) []byte { return mp4Box("mdat", payload) }

// mp4StsdPayload: entry_count(4) + sample entry (size4+type4+8 filler).
func mp4StsdPayload(entryType string) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p[0:4], 1)
	se := make([]byte, 16)
	binary.BigEndian.PutUint32(se[0:4], 16)
	copy(se[4:8], entryType)
	return append(p, se...)
}

// buildMP4Track builds a trak box for one track with the given sample table.
func buildMP4Track(entryType string, timescale uint32, sampleCount, sampleDelta, sampleSize uint32, chunkOff uint32, keySample uint32) []byte {
	// stts: count + delta
	sttsP := make([]byte, 4)
	binary.BigEndian.PutUint32(sttsP[0:4], 1)
	row := make([]byte, 8)
	binary.BigEndian.PutUint32(row[0:4], sampleCount)
	binary.BigEndian.PutUint32(row[4:8], sampleDelta)
	sttsP = append(sttsP, row...)
	// stsz: uniform=0, count, sizes
	stszP := make([]byte, 8)
	binary.BigEndian.PutUint32(stszP[0:4], 0)
	binary.BigEndian.PutUint32(stszP[4:8], sampleCount)
	for i := uint32(0); i < sampleCount; i++ {
		s := make([]byte, 4)
		binary.BigEndian.PutUint32(s, sampleSize)
		stszP = append(stszP, s...)
	}
	// stsc: 1 entry, firstChunk=1, samplesPerChunk=sampleCount
	stscP := make([]byte, 4)
	binary.BigEndian.PutUint32(stscP[0:4], 1)
	srow := make([]byte, 12)
	binary.BigEndian.PutUint32(srow[0:4], 1)
	binary.BigEndian.PutUint32(srow[4:8], sampleCount)
	binary.BigEndian.PutUint32(srow[8:12], 1)
	stscP = append(stscP, srow...)
	// stco: 1 offset
	stcoP := make([]byte, 4)
	binary.BigEndian.PutUint32(stcoP[0:4], 1)
	off := make([]byte, 4)
	binary.BigEndian.PutUint32(off, chunkOff)
	stcoP = append(stcoP, off...)
	// stss: 1 sync sample
	stssP := make([]byte, 4)
	binary.BigEndian.PutUint32(stssP[0:4], 1)
	k := make([]byte, 4)
	binary.BigEndian.PutUint32(k, keySample)
	stssP = append(stssP, k...)

	stblChildren := bytes.Join([][]byte{
		mp4FullBox("stsd", mp4StsdPayload(entryType)),
		mp4FullBox("stts", sttsP),
		mp4FullBox("stsz", stszP),
		mp4FullBox("stsc", stscP),
		mp4FullBox("stco", stcoP),
		mp4FullBox("stss", stssP),
	}, nil)
	stbl := mp4Box("stbl", stblChildren)
	minf := mp4Box("minf", stbl)
	// mdhd v0 body: creation(4)+mod(4)+timescale(4)+duration(4)+lang(2)+predef(2)
	mdhdBody := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdBody[8:12], timescale)
	mdhd := mp4FullBox("mdhd", mdhdBody)
	mdia := mp4Box("mdia", bytes.Join([][]byte{mdhd, minf}, nil))
	tkhd := mp4FullBox("tkhd", make([]byte, 80))
	return mp4Box("trak", bytes.Join([][]byte{tkhd, mdia}, nil))
}

// buildFullMP4 composes ftyp+moov+mdat and returns the bytes + mdat payload offset.
func buildFullMP4(traks [][]byte, mdatPayload []byte) ([]byte, int) {
	var out bytes.Buffer
	out.Write(mp4Ftyp())
	mvhd := mp4FullBox("mvhd", make([]byte, 100))
	moov := mp4Box("moov", bytes.Join(append([][]byte{mvhd}, traks...), nil))
	out.Write(moov)
	off := out.Len() + 8
	out.Write(mp4Mdat(mdatPayload))
	return out.Bytes(), off
}

func TestMergeMP4ToWebM(t *testing.T) {
	dir := t.TempDir()
	// One VP9 video track: 2 samples of 4 bytes each at 0ms and 33ms.
	mdatPayload := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB}
	trak := buildMP4Track("vp09", 1000, 2, 33, 4, 0, 1) // offset patched below
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("vp09", 1000, 2, 33, 4, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)

	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.webm")
	if err := Merge(context.Background(), []string{in}, out, DefaultConfig()); err != nil {
		t.Fatalf("Merge MP4->WebM: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("webm")) {
		t.Error("output missing webm doctype")
	}
	if !bytes.Contains(b, []byte("V_VP9")) {
		t.Error("output missing V_VP9 codec id")
	}
	// The VP9 keyframe sample payload should appear in the output.
	if !bytes.Contains(b, []byte{0xAA, 0xAA, 0xAA, 0xAA}) {
		t.Error("output missing video sample payload")
	}
}

func TestMergeMP4ToMKV(t *testing.T) {
	dir := t.TempDir()
	mdatPayload := []byte{0xCC, 0xCC, 0xCC, 0xCC}
	trak := buildMP4Track("av01", 1000, 1, 33, 4, 0, 1)
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("av01", 1000, 1, 33, 4, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)
	in := filepath.Join(dir, "in.mp4")
	os.WriteFile(in, data, 0o644)
	out := filepath.Join(dir, "out.mkv")
	if err := Merge(context.Background(), []string{in}, out, DefaultConfig()); err != nil {
		t.Fatalf("Merge MP4->MKV: %v", err)
	}
	b, _ := os.ReadFile(out)
	if !bytes.Contains(b, []byte("matroska")) {
		t.Error("MKV output missing matroska doctype")
	}
	if !bytes.Contains(b, []byte("V_AV1")) {
		t.Error("output missing V_AV1 codec id")
	}
}

func TestMergeMP4AACRejectedInWebM(t *testing.T) {
	// MP4 with AAC audio: AAC is read-only; puremux cannot mux it into WebM.
	// The gate must reject it at AddTrack time (ErrUnsupportedCodec surfaces
	// as a write error), not silently produce a broken file.
	dir := t.TempDir()
	mdatPayload := []byte{0xDD, 0xDD, 0xDD, 0xDD}
	trak := buildMP4Track("mp4a", 1000, 1, 20, 4, 0, 1)
	data, off := buildFullMP4([][]byte{trak}, mdatPayload)
	trak = buildMP4Track("mp4a", 1000, 1, 20, 4, uint32(off), 1)
	data, _ = buildFullMP4([][]byte{trak}, mdatPayload)
	in := filepath.Join(dir, "in.mp4")
	os.WriteFile(in, data, 0o644)
	out := filepath.Join(dir, "out.webm")
	err := Merge(context.Background(), []string{in}, out, DefaultConfig())
	if err == nil {
		t.Error("expected error muxing AAC into WebM, got nil")
	}
	// The error should not be ErrUnsupportedInput/Output (those are container
	// gates); it is a per-track codec rejection.
	if errors.Is(err, ErrUnsupportedInput) || errors.Is(err, ErrUnsupportedOutput) {
		t.Errorf("AAC rejection should be per-track, not a container gate: %v", err)
	}
}
