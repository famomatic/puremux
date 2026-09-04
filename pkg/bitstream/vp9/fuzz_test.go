package vp9

import "testing"

func FuzzVP9Configs(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 0})
	f.Add([]byte{1, 1, 0, 2, 1, 10, 3, 1, 8, 4, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = ValidateVPCC(data)
		_ = ValidateFeatureMetadata(data)
		_, _ = FeatureMetadataFromVPCC(data)
		_, _ = VPCCFromFeatureMetadata(data)
	})
}
