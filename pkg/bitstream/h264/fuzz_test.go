package h264

import "testing"

func FuzzParseAVCC(f *testing.F) {
	f.Add([]byte{1, 100, 0, 31, 0xff, 0xe1, 0, 2, 0x67, 0x64, 1, 0, 1, 0x68,
		0xfd, 0xf8, 0xf8, 0})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseAVCC(data)
	})
}
