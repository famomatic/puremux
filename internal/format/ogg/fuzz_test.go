package ogg

import "testing"

func FuzzOpusHeaders(f *testing.F) {
	f.Add(append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0))
	f.Add(append([]byte("OpusTags"), 0, 0, 0, 0, 0, 0, 0, 0))
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseOpusHead(data)
		_, _ = parseOpusTags(data)
	})
}
