package vp9

import (
	"bytes"
	"testing"
)

func TestVPCCFeatureMetadataRoundTrip(t *testing.T) {
	// VP9 MP4 fields are byte-oriented except byte 6, packed MSB-first:
	// bitDepth=10 (1010), chroma=1 (001), fullRange=0 => A2.
	vpcc := []byte{1, 0, 0, 0, 2, 10, 0xa2, 1, 1, 1, 0, 0}
	wantFeatures := []byte{1, 1, 2, 2, 1, 10, 3, 1, 10, 4, 1, 1}
	features, err := FeatureMetadataFromVPCC(vpcc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(features, wantFeatures) {
		t.Fatalf("features = %x, want %x", features, wantFeatures)
	}
	got, err := VPCCFromFeatureMetadata(features)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, vpcc) {
		t.Fatalf("vpcC = %x, want %x", got, vpcc)
	}
}

func TestVPCCBoundaries(t *testing.T) {
	valid := []byte{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 0}
	cases := [][]byte{
		nil,
		valid[:11],
		{0, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 0},
		{1, 0, 0, 1, 0, 10, 0x82, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 4, 10, 0x82, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 0, 12, 0x82, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 0, 10, 0x92, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 0, 10, 0x88, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 1, 10, 0x82, 1, 1, 1, 0, 0},
		{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 0, 0, 0},
		{1, 0, 0, 0, 0, 10, 0x82, 1, 1, 1, 0, 1, 0},
	}
	if err := ValidateVPCC(valid); err != nil {
		t.Fatalf("valid vpcC rejected: %v", err)
	}
	for _, data := range cases {
		if err := ValidateVPCC(data); err == nil {
			t.Fatalf("invalid vpcC accepted: %x", data)
		}
	}
}

func TestFeatureMetadataBoundaries(t *testing.T) {
	valid := []byte{1, 1, 0, 2, 1, 10, 3, 1, 8, 4, 1, 1}
	if err := ValidateFeatureMetadata(nil); err != nil {
		t.Fatalf("optional empty feature list rejected: %v", err)
	}
	if err := ValidateFeatureMetadata(valid); err != nil {
		t.Fatalf("valid feature list rejected: %v", err)
	}
	for _, data := range [][]byte{
		{1},
		{0x81, 1, 0},
		{5, 1, 0},
		{1, 2, 0},
		{1, 1},
		{1, 1, 0, 1, 1, 0},
		{1, 1, 4},
		{2, 1, 12},
		{3, 1, 9},
		{4, 1, 4},
		{1, 1, 1, 2, 1, 10, 3, 1, 8, 4, 1, 1},
	} {
		if err := ValidateFeatureMetadata(data); err == nil {
			t.Fatalf("invalid feature list accepted: %x", data)
		}
	}
	if _, err := VPCCFromFeatureMetadata(valid[:9]); err == nil {
		t.Fatal("incomplete feature list converted to vpcC")
	}
}
