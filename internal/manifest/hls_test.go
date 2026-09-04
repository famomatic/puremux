package manifest

import (
	"net/url"
	"testing"
	"time"
)

func TestParseHLSMasterAndMediaRFC8216Fields(t *testing.T) {
	base, _ := url.Parse("https://media.example/master.m3u8")
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"English\",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE=\"en\",URI=\"audio/index.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=128000,CODECS=\"mp4a.40.2\",AUDIO=\"aac\"\nvideo/main.m3u8\n"
	p, err := ParseHLS(base, []byte(master), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Master || len(p.Variants) != 1 || p.Variants[0].Bandwidth != 128000 || p.Variants[0].URI != "https://media.example/video/main.m3u8" || len(p.Renditions) != 1 || !p.Renditions[0].Default || p.Renditions[0].URI != "https://media.example/audio/index.m3u8" {
		t.Fatalf("unexpected master: %+v", p)
	}

	media := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-MAP:URI=\"all.mp4\",BYTERANGE=\"100@0\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x0000000000000000000000000000002A\n#EXTINF:5.5,first\n#EXT-X-BYTERANGE:20@100\nall.mp4\n#EXT-X-DISCONTINUITY\n#EXTINF:6,second\n#EXT-X-BYTERANGE:30\nall.mp4\n#EXT-X-ENDLIST\n"
	p, err = ParseHLS(base, []byte(media), 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Master || !p.EndList || p.MediaSequence != 42 || p.TargetDuration != 6*time.Second || len(p.Segments) != 2 {
		t.Fatalf("unexpected media playlist: %+v", p)
	}
	first, second := p.Segments[0], p.Segments[1]
	if first.Sequence != 42 || first.Duration != 5500*time.Millisecond || first.Range.Offset != 100 || first.Range.Length != 20 || first.Map == nil || first.Map.Range.Length != 100 || first.Key == nil || !first.Key.HasIV || first.Key.IV[15] != 0x2a {
		t.Fatalf("unexpected first segment: %+v", first)
	}
	if second.Sequence != 43 || second.Range.Offset != 120 || second.Range.Length != 30 || !second.Discontinuity {
		t.Fatalf("unexpected second segment: %+v", second)
	}
}

func TestParseHLSBoundaries(t *testing.T) {
	base, _ := url.Parse("https://example.test/root.m3u8")
	cases := []string{
		"",
		"not-m3u8\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=x\na.m3u8\n",
		"#EXTM3U\n#EXT-X-BYTERANGE:10\na.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key\"\n#EXTINF:1,\na.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0xzz\n#EXTINF:1,\na.ts\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"x\",BYTERANGE=\"0@0\"\n#EXTINF:1,\nx\n",
		"#EXTM3U\na.ts\n",
		"#EXTM3U\n#EXTINF:NaN,\na.ts\n",
		"#EXTM3U\n#EXTINF:+Inf,\na.ts\n",
		"#EXTM3U\n#EXTINF:0,\na.ts\n",
	}
	for i, input := range cases {
		if _, err := ParseHLS(base, []byte(input), 2); err == nil {
			t.Fatalf("case %d accepted malformed playlist", i)
		}
	}
	tooMany := "#EXTM3U\n#EXTINF:1,\na.ts\n#EXTINF:1,\nb.ts\n"
	if _, err := ParseHLS(base, []byte(tooMany), 1); err == nil {
		t.Fatal("entry limit was not enforced")
	}
	tooManyRenditions := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"one\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"two\"\n"
	if _, err := ParseHLS(base, []byte(tooManyRenditions), 1); err == nil {
		t.Fatal("rendition limit was not enforced")
	}
}

func TestHLSMapCapturesDeclarationKey(t *testing.T) {
	base, _ := url.Parse("https://example.test/root.m3u8")
	input := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"map-key\",IV=0x01\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:1,\nsegment.m4s\n"
	p, err := ParseHLS(base, []byte(input), 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Segments[0].Map == nil || p.Segments[0].Map.Key == nil || p.Segments[0].Map.Key.URI != "https://example.test/map-key" || p.Segments[0].Key != nil {
		t.Fatalf("map key state was not preserved: %+v", p.Segments[0])
	}
}
