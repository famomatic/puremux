package media

import "testing"

func FuzzVorbisComments(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseVorbisComments(data, make(map[string]string))
	})
}
