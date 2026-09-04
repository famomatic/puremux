package aac

import "testing"

func FuzzAACParsers(f *testing.F) {
	f.Add([]byte{0x12, 0x10}) // AAC-LC, 44.1 kHz, stereo; MSB-first ASC.
	f.Add([]byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x3f, 0xfc})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseASC(data)
		_, _ = ParseADTS(data)
	})
}
