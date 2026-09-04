package nal

import "testing"

func FuzzNALFraming(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, 0x65, 0x88}, byte(2))
	f.Add([]byte{0, 0, 0, 1, 0x65}, byte(2))
	f.Add([]byte(nil), byte(0))
	f.Fuzz(func(t *testing.T, data []byte, selector byte) {
		lengthSize := [...]int{1, 2, 4}[int(selector)%3]
		_, _ = LengthPrefixedToAnnexB(data, lengthSize)
		_, _ = AnnexBToLengthPrefixed(data, lengthSize)
	})
}
