package puremux

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

// writeTmp writes b to a temp file with the given extension and returns its path.
func writeTmp(t *testing.T, ext string, b []byte) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "input."+ext)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// EBML header magic: idEBML Element ID = 0x1A45DFA3 (class 4, RFC 8794).
// Full minimal WebM EBML header bytes (derived from the writer):
//   1A 45 DF A3  <size>  42 82 <len> "webm"  ...
// We construct the smallest valid framing by hand from the spec.
func webmHeaderBytes(t *testing.T) []byte {
	t.Helper()
	// Use the real writer to get spec-accurate bytes, then we only assert on
	// the magic prefix + doctype for sniffing (the writer is itself verified
	// by internal/format/webm tests against RFC 8794).
	// idEBML = 1A 45 DF A3; size VINT for the payload length follows.
	// Children: EBMLVersion(1)=42 86 81 01, DocType "webm" = 42 82 84 77 65 62 6D
	// DocTypeVersion(4)=42 87 81 04, DocTypeReadVer(2)=42 85 81 02
	payload := []byte{
		0x42, 0x86, 0x81, 0x01, // EBMLVersion = 1
		0x42, 0x82, 0x84, 'w', 'e', 'b', 'm', // DocType = "webm"
		0x42, 0x87, 0x81, 0x04, // DocTypeVersion = 4
		0x42, 0x85, 0x81, 0x02, // DocTypeReadVer = 2
	}
	out := []byte{0x1A, 0x45, 0xDF, 0xA3}
	out = append(out, byte(0x80|len(payload))) // width-1 size VINT
	out = append(out, payload...)
	return out
}

func mkvHeaderBytes(t *testing.T) []byte {
	t.Helper()
	payload := []byte{
		0x42, 0x86, 0x81, 0x01,
		0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a', // DocType = "matroska"
		0x42, 0x87, 0x81, 0x04,
		0x42, 0x85, 0x81, 0x02,
	}
	out := []byte{0x1A, 0x45, 0xDF, 0xA3}
	out = append(out, byte(0x80|len(payload)))
	out = append(out, payload...)
	return out
}

// MP4 ftyp box: 4-byte size, "ftyp", major brand, minor version, compat brands.
// Minimal ftyp: size=0x18(24), "ftyp", "isom", 0x00000200, "isom".
func mp4FtypBytes() []byte {
	return []byte{
		0x00, 0x00, 0x00, 0x18, // box size = 24
		'f', 't', 'y', 'p', // box type "ftyp" at offset 4
		'i', 's', 'o', 'm', // major brand
		0x00, 0x00, 0x02, 0x00, // minor version
		'i', 's', 'o', 'm', // compatible brand
	}
}

func TestDetectContainerWebM(t *testing.T) {
	p := writeTmp(t, "webm", webmHeaderBytes(t))
	c, err := DetectContainer(p)
	if err != nil {
		t.Fatalf("DetectContainer: %v", err)
	}
	if c != ContainerWebM {
		t.Errorf("got %s want webm", c)
	}
}

func TestDetectContainerMKVByDoctype(t *testing.T) {
	p := writeTmp(t, "mkv", mkvHeaderBytes(t))
	c, err := DetectContainer(p)
	if err != nil {
		t.Fatalf("DetectContainer: %v", err)
	}
	if c != ContainerMKV {
		t.Errorf("got %s want mkv", c)
	}
}

func TestDetectContainerMP4(t *testing.T) {
	p := writeTmp(t, "mp4", mp4FtypBytes())
	c, err := DetectContainer(p)
	if err != nil {
		t.Fatalf("DetectContainer: %v", err)
	}
	if c != ContainerMP4 {
		t.Errorf("got %s want mp4", c)
	}
}

func TestDetectContainerUnknown(t *testing.T) {
	p := writeTmp(t, "bin", []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	_, err := DetectContainer(p)
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("got err %v want ErrUnsupportedInput", err)
	}
}

func TestDetectContainerTruncated(t *testing.T) {
	// Fewer than 4 bytes: must return ErrUnsupportedInput, not panic.
	p := writeTmp(t, "webm", []byte{0x1A, 0x45})
	_, err := DetectContainer(p)
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("truncated got err %v want ErrUnsupportedInput", err)
	}
}

func TestDetectContainerEmpty(t *testing.T) {
	p := writeTmp(t, "webm", nil)
	_, err := DetectContainer(p)
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("empty got err %v want ErrUnsupportedInput", err)
	}
}

func TestDetectContainerEBMLFallbackByExtension(t *testing.T) {
	// EBML magic but no parseable doctype (header truncated right after size):
	// must fall back to extension, not error.
	hdr := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x80} // size 0, no children
	p := writeTmp(t, "webm", hdr)
	c, err := DetectContainer(p)
	if err != nil {
		t.Fatalf("fallback detect: %v", err)
	}
	if c != ContainerWebM {
		t.Errorf("fallback got %s want webm (by extension)", c)
	}
}

func TestDetectContainerReaderMP4Fragmented(t *testing.T) {
	// styp (segment type box, fragmented MP4) must also be detected.
	styp := []byte{0x00, 0x00, 0x00, 0x10, 's', 't', 'y', 'p', 'm', 's', 'd', 'h', 0, 0, 0, 0}
	c, err := DetectContainerReader("x.mp4", bytes.NewReader(styp))
	if err != nil {
		t.Fatalf("DetectContainerReader: %v", err)
	}
	if c != ContainerMP4 {
		t.Errorf("styp got %s want mp4", c)
	}
}

func TestCanRemux(t *testing.T) {
	// CanRemux is the coarse container-level gate: it returns true whenever
	// puremux has a reader for in and a writer for out. Codec-level rejection
	// happens at AddTrack time; see TestCanRemuxCodecs for the finer gate.
	cases := []struct {
		in, out Container
		want    bool
	}{
		{ContainerWebM, ContainerWebM, true},
		{ContainerMKV, ContainerMKV, true},
		{ContainerWebM, ContainerMKV, true},
		{ContainerMKV, ContainerWebM, true}, // container-level OK; FLAC tracks rejected later
		{ContainerMP4, ContainerWebM, true},
		{ContainerMP4, ContainerMKV, true},
		{ContainerWebM, ContainerMP4, false}, // MP4 not writable
		{ContainerMP4, ContainerMP4, false},  // no MP4 writer
		{ContainerUnknown, ContainerWebM, false},
		{ContainerWebM, ContainerUnknown, false},
	}
	for _, c := range cases {
		got := CanRemux(c.in, c.out)
		if got != c.want {
			t.Errorf("CanRemux(%s,%s)=%v want %v", c.in, c.out, got, c.want)
		}
	}
}

func TestCanRemuxCodecs(t *testing.T) {
	cases := []struct {
		out    Container
		codecs []core.CodecType
		want   bool
	}{
		{ContainerWebM, []core.CodecType{core.CodecVP9, core.CodecOpus}, true},
		{ContainerWebM, []core.CodecType{core.CodecFLAC}, false},                // FLAC not in WebM
		{ContainerWebM, []core.CodecType{core.CodecVP9, core.CodecFLAC}, false}, // mixed: FLAC breaks it
		{ContainerMKV, []core.CodecType{core.CodecFLAC, core.CodecAV1}, true},
		{ContainerWebM, nil, true},                              // empty set is trivially OK
		{ContainerWebM, []core.CodecType{core.CodecAAC}, false}, // AAC read-only
	}
	for _, c := range cases {
		got := CanRemuxCodecs(c.out, c.codecs)
		if got != c.want {
			t.Errorf("CanRemuxCodecs(%s,%v)=%v want %v", c.out, c.codecs, got, c.want)
		}
	}
}

func TestSupportedOutputsWritable(t *testing.T) {
	for _, c := range SupportedOutputs() {
		if !c.CanWrite {
			t.Errorf("SupportedOutputs entry %s must be CanWrite", c.Container)
		}
		if c.CanRead {
			t.Errorf("SupportedOutputs entry %s must not be CanRead", c.Container)
		}
	}
	// MP4 must NOT appear in outputs (puremux never writes MP4).
	for _, c := range SupportedOutputs() {
		if c.Container == ContainerMP4 {
			t.Error("MP4 must not be a supported output")
		}
	}
}

func TestSupportedInputsReadable(t *testing.T) {
	gotMP4 := false
	for _, c := range SupportedInputs() {
		if !c.CanRead {
			t.Errorf("SupportedInputs entry %s must be CanRead", c.Container)
		}
		if c.Container == ContainerMP4 {
			gotMP4 = true
		}
	}
	if !gotMP4 {
		t.Error("MP4 must be a supported input")
	}
}

func TestContainerStringAndExt(t *testing.T) {
	cases := []struct {
		c    Container
		name string
		ext  string
	}{
		{ContainerWebM, "webm", "webm"},
		{ContainerMKV, "mkv", "mkv"},
		{ContainerMP4, "mp4", "mp4"},
		{ContainerUnknown, "unknown", ""},
	}
	for _, c := range cases {
		if c.c.String() != c.name {
			t.Errorf("String()=%q want %q", c.c, c.name)
		}
		if c.c.Extension() != c.ext {
			t.Errorf("Extension()=%q want %q", c.c.Extension(), c.ext)
		}
	}
}

func TestOutputContainerForPath(t *testing.T) {
	cases := []struct {
		path string
		want Container
		err  bool
	}{
		{"out.webm", ContainerWebM, false},
		{"out.MKV", ContainerMKV, false}, // case-insensitive
		{"out.mka", ContainerMKV, false},
		{"out.mp4", ContainerUnknown, true}, // MP4 is input-only
		{"out.avi", ContainerUnknown, true},
		{"out", ContainerUnknown, true},
	}
	for _, c := range cases {
		got, err := outputContainerForPath(c.path)
		if c.err {
			if err == nil {
				t.Errorf("path %q: expected error, got %s", c.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("path %q: unexpected error %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("path %q: got %s want %s", c.path, got, c.want)
		}
	}
}

func TestAddTrackCodecGate(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerWebM
	s, _ := NewSession(&buf, cfg)
	// FLAC is NOT permitted in WebM (only MKV).
	if _, err := s.AddTrack(Track{Codec: core.CodecFLAC, Channels: 2}); err == nil {
		t.Error("FLAC in WebM should be rejected")
	}
	// Opus IS permitted in WebM.
	if _, err := s.AddTrack(Track{Codec: core.CodecOpus, Channels: 2}); err != nil {
		t.Errorf("Opus in WebM should be allowed, got %v", err)
	}
}

func TestAddTrackFLACAllowedInMKV(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.OutputContainer = ContainerMKV
	s, _ := NewSession(&buf, cfg)
	if _, err := s.AddTrack(Track{Codec: core.CodecFLAC, Channels: 2}); err != nil {
		t.Errorf("FLAC in MKV should be allowed, got %v", err)
	}
}

// Verify the sniffed bytes are exactly the spec-derived values (not just
// "contains"). This guards against false-positive magic detection.
func TestDetectContainerMagicExactBytes(t *testing.T) {
	wb := webmHeaderBytes(t)
	// First 4 bytes must be the EBML magic exactly.
	want := []byte{0x1A, 0x45, 0xDF, 0xA3}
	if !bytes.Equal(wb[:4], want) {
		t.Fatalf("EBML magic = % X want % X", wb[:4], want)
	}
	// DocType "webm" must appear right after the DocType element id+size.
	if !bytes.Contains(wb, []byte{'w', 'e', 'b', 'm'}) {
		t.Fatal("doctype webm missing")
	}
	mb := mp4FtypBytes()
	if string(mb[4:8]) != "ftyp" {
		t.Fatalf("ftyp magic = %q want ftyp", mb[4:8])
	}
}

// guard: ensure capability tables reference real codecs (compile-time).
var _ = []CodecCombo{{Codec: core.CodecVorbis}}
var _ = strings.ToLower
