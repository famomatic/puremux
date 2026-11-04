package manifest

import (
	"net/url"
	"testing"
)

func TestAudit20260906HLSImplicitRangeAfterWholeSegment(t *testing.T) {
	base, _ := url.Parse("https://example.test/index.m3u8")
	data := []byte("#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n#EXT-X-BYTERANGE:4@0\na.mp4\n#EXTINF:1,\nb.mp4\n#EXTINF:1,\n#EXT-X-BYTERANGE:4\na.mp4\n#EXT-X-ENDLIST\n")
	if p, err := ParseHLS(base, data, 10); err == nil {
		t.Fatalf("RFC8216 4.3.2.2 violation accepted; stale range=%+v", p.Segments[2].Range)
	}
}
