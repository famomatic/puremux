package mp4

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"

	mp4ff "github.com/Eyevinn/mp4ff/mp4"
	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/nal"
)

func TestMP4FFParsesProgressiveRealH264Fixture(t *testing.T) {
	fixture, config, sample := loadMP4FFH264Fixture(t)
	if len(fixture) != 184 {
		t.Fatalf("decoded attributed fixture size = %d, want 184", len(fixture))
	}
	f, err := os.CreateTemp(t.TempDir(), "mp4ff-progressive-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := NewProgressiveWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddTrack(OutputTrack{ID: 1, Codec: core.CodecH264, TimeScale: 90_000,
		Width: 1280, Height: 720, ConfigType: "avcC", Config: config}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSample(OutputSample{TrackID: 1, DTS: 0, PTS: 3_000, Duration: 3_000,
		Keyframe: true, Data: sample}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4ff.DecodeFile(f)
	if err != nil {
		t.Fatalf("mp4ff progressive parse: %v", err)
	}
	if parsed.Ftyp == nil || parsed.Moov == nil || parsed.Mdat == nil || parsed.IsFragmented() {
		t.Fatalf("mp4ff progressive structure: ftyp=%v moov=%v mdat=%v fragmented=%v",
			parsed.Ftyp != nil, parsed.Moov != nil, parsed.Mdat != nil, parsed.IsFragmented())
	}
	if len(parsed.Moov.Traks) != 1 {
		t.Fatalf("mp4ff tracks = %d", len(parsed.Moov.Traks))
	}
	stbl := parsed.Moov.Traks[0].Mdia.Minf.Stbl
	if parsed.Moov.Traks[0].Mdia.Mdhd.Duration != 3_000 {
		t.Fatalf("mp4ff mdhd duration = %d, want decode duration 3000", parsed.Moov.Traks[0].Mdia.Mdhd.Duration)
	}
	if stbl.Stsd.AvcX == nil || stbl.Stsz.SampleNumber != 1 || stbl.Ctts == nil || stbl.Ctts.Version != 1 {
		t.Fatalf("mp4ff sample tables: avc=%v samples=%d ctts=%v",
			stbl.Stsd.AvcX != nil, stbl.Stsz.SampleNumber, stbl.Ctts)
	}
	var copied bytes.Buffer
	if err := parsed.CopySampleData(&copied, nil, parsed.Moov.Traks[0], 1, 1, nil); err != nil {
		t.Fatalf("mp4ff progressive sample offset: %v", err)
	}
	if !bytes.Equal(copied.Bytes(), sample) {
		t.Fatalf("mp4ff progressive sample differs: got %d bytes, want %d", copied.Len(), len(sample))
	}
}

func TestMP4FFParsesFragmentedRealH264Fixture(t *testing.T) {
	_, config, sample := loadMP4FFH264Fixture(t)
	var output bytes.Buffer
	w, err := NewFragmentedWriter(&output, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddTrack(OutputTrack{ID: 7, Codec: core.CodecH264, TimeScale: 90_000,
		Width: 1280, Height: 720, ConfigType: "avcC", Config: config}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSample(OutputSample{TrackID: 7, DTS: 0, PTS: 0, Duration: 3_000,
		Keyframe: true, Data: sample}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4ff.DecodeFile(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("mp4ff fragmented parse: %v", err)
	}
	if !parsed.IsFragmented() || parsed.Init == nil || parsed.Moov == nil || parsed.Moov.Mvex == nil ||
		len(parsed.Segments) != 1 || len(parsed.Segments[0].Fragments) != 1 {
		t.Fatalf("mp4ff fragmented structure: fragmented=%v init=%v mvex=%v segments=%d",
			parsed.IsFragmented(), parsed.Init != nil, parsed.Moov != nil && parsed.Moov.Mvex != nil, len(parsed.Segments))
	}
	fragment := parsed.Segments[0].Fragments[0]
	if fragment.Moof == nil || fragment.Mdat == nil || len(fragment.Moof.Trafs) != 1 ||
		len(fragment.Moof.Trafs[0].Truns) != 1 || len(fragment.Moof.Trafs[0].Truns[0].Samples) != 1 {
		t.Fatalf("mp4ff fragment boxes are incomplete")
	}
	fullSamples, err := fragment.GetFullSamples(nil)
	if err != nil {
		t.Fatalf("mp4ff fragmented sample offset: %v", err)
	}
	if len(fullSamples) != 1 || fullSamples[0].Dur != 3_000 || fullSamples[0].DecodeTime != 0 ||
		fullSamples[0].PresentationTime() != 0 || !bytes.Equal(fullSamples[0].Data, sample) {
		t.Fatalf("mp4ff fragmented sample = %+v", fullSamples)
	}
	for _, cut := range []int{0, 4, len(output.Bytes()) - 1} {
		truncated, err := mp4ff.DecodeFile(bytes.NewReader(output.Bytes()[:cut]))
		if err == nil && truncated.Ftyp != nil && truncated.Moov != nil && truncated.IsFragmented() &&
			len(truncated.Segments) > 0 && len(truncated.Segments[0].Fragments) > 0 &&
			truncated.Segments[0].Fragments[0].Mdat != nil {
			t.Fatalf("mp4ff reported truncated output length %d as structurally complete", cut)
		}
	}
}

func loadMP4FFH264Fixture(t *testing.T) (fixture, config, sample []byte) {
	t.Helper()
	encoded, err := os.ReadFile("testdata/mp4ff_blackframe.264.b64")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	// The attributed Annex-B file contains SPS at [4:29], PPS at [33:39],
	// and one IDR beginning with the 3-byte start code at 39. These boundaries
	// are independently rechecked so a changed fixture cannot silently make the
	// test's avcC record describe different bytes.
	if len(fixture) < 43 || !bytes.Equal(fixture[:4], []byte{0, 0, 0, 1}) ||
		!bytes.Equal(fixture[29:33], []byte{0, 0, 0, 1}) ||
		!bytes.Equal(fixture[39:42], []byte{0, 0, 1}) ||
		fixture[4]&0x1f != 7 || fixture[33]&0x1f != 8 || fixture[42]&0x1f != 5 {
		t.Fatal("attributed H.264 fixture NAL boundaries changed")
	}
	sps, pps := fixture[4:29], fixture[33:39]
	config = []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, 0, byte(len(sps))}
	config = append(config, sps...)
	config = append(config, 1, 0, byte(len(pps)))
	config = append(config, pps...)
	// The fixture declares High Profile. ISO/IEC 14496-15 therefore requires
	// the MSB-first profile extension: 4:2:0 chroma (1), 8-bit luma/chroma
	// (minus-eight fields zero), and zero SPS extension NAL units.
	config = append(config, 0xfd, 0xf8, 0xf8, 0)
	sample, err = nal.AnnexBToLengthPrefixed(fixture[39:], 4)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, config, sample
}
