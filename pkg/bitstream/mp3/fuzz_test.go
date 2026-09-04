package mp3

import "testing"

func FuzzParseHeader(f *testing.F) {
	f.Add([]byte{0xff, 0xfb, 0x90, 0x64})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseHeader(data)
	})
}
