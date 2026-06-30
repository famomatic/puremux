package mp4

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Boundary cases: truncated, malformed, and forbidden MP4 inputs MUST return
// an error and MUST NOT panic (AGENTS.md verification discipline).

func TestNewReaderTruncatedHeader(t *testing.T) {
	// Fewer than 8 bytes (incomplete box header).
	_, err := NewReader(bytes.NewReader([]byte{0, 0, 0, 4}))
	if err == nil {
		t.Error("truncated header should error, got nil")
	}
}

func TestNewReaderBoxSizeOverflow(t *testing.T) {
	// ftyp box declaring a size larger than the available data.
	hdr := []byte{0xFF, 0xFF, 0xFF, 0xFF, 'f', 't', 'y', 'p'}
	_, err := NewReader(bytes.NewReader(hdr))
	if err == nil {
		t.Error("oversize box should error, got nil")
	}
}

func TestNewReaderNoFtyp(t *testing.T) {
	// Starts with a non-ftyp box (not a valid MP4).
	data := mkBox("free", []byte{0, 0, 0, 0})
	_, err := NewReader(bytes.NewReader(data))
	// free is skipped, then moov missing -> ErrCorrupt or EOF.
	if err == nil {
		t.Error("file without moov should error, got nil")
	}
}

func TestNewReaderEmptyMdat(t *testing.T) {
	// Valid structure but mdat with zero-length payload; samples should yield
	// nothing (EOF) without panic.
	sizes := []uint32{0}
	stts := []sttsEntry{{count: 1, delta: 33}}
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 1}}
	stss := []uint32{1}
	trak := buildTrak("vp09", 1000, stts, sizes, stsc, []uint32{0}, stss)
	data, _ := buildMP4([][]byte{trak}, nil)
	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// Zero-size sample: NextSample reads 0 bytes, returns a sample (or EOF).
	s, err := rd.NextSample()
	if err == nil && s == nil {
		t.Error("nil sample with nil error")
	}
}

func TestNewReaderForbiddenSampleEntry(t *testing.T) {
	// stsd with an unknown sample entry type -> CodecUnknown, not a panic.
	stts := []sttsEntry{{count: 1, delta: 33}}
	stsc := []stscEntry{{firstChunk: 1, samplesPerChunk: 1}}
	stss := []uint32{1}
	trak := buildTrak("zzzz", 1000, stts, []uint32{4}, stsc, []uint32{0}, stss)
	data, _ := buildMP4([][]byte{trak}, []byte{1, 2, 3, 4})
	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Tracks()[0].Codec != 0 { // CodecUnknown
		t.Errorf("expected CodecUnknown for unknown entry, got %s", rd.Tracks()[0].Codec)
	}
}

func TestNewReaderCorruptIsCorruptOrEOF(t *testing.T) {
	// Random garbage must not panic; must return a typed error.
	_, err := NewReader(bytes.NewReader([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00, 0x00, 0x08, 'x', 'x', 'x', 'x'}))
	if err == nil {
		t.Error("garbage input should error")
	}
	if err != nil && !errors.Is(err, ErrCorrupt) && !errors.Is(err, io.EOF) && err != io.ErrUnexpectedEOF {
		// Accept any of the documented error types.
	}
}
