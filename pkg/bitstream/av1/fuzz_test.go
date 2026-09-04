package av1

import "testing"

func FuzzValidateConfig(f *testing.F) {
	f.Add([]byte{0x81, 0, 0, 0})
	f.Add([]byte{0x81, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = ValidateConfig(data)
	})
}
