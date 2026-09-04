package opus

import "testing"

func FuzzOpusConfigurations(f *testing.F) {
	f.Add(append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0))
	f.Add([]byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 0})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseHead(data)
		_, _ = ParseDOPS(data)
		_, _ = DOPSFromHead(data)
	})
}
