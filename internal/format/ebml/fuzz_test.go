package ebml

import "testing"

func FuzzDecodeHeaders(f *testing.F) {
	f.Add([]byte{0x1a, 0x45, 0xdf, 0xa3})
	f.Add([]byte{0x81})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = DecodeElementID(data)
		value, width, err := DecodeVINT(data)
		_ = value
		if err == nil {
			_ = IsUnknownSize(data, width)
		}
	})
}
