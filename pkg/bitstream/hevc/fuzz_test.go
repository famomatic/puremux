package hevc

import "testing"

func FuzzParseHVCC(f *testing.F) {
	f.Add(testHVCCRecord())
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseHVCC(data)
	})
}
