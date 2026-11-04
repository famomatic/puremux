package av1

import "testing"

func TestAudit20260906AV1MaximumLevel(t *testing.T) {
	// AV1C marker1/version1; profile0/level31; 8bit420 unknown chroma position.
	if err := ValidateConfig([]byte{0x81, 0x1f, 0x0c, 0}); err != nil {
		t.Fatalf("valid maximum-parameters level31 rejected: %v", err)
	}
}
func TestAudit20260906AV1InvalidBitDepth(t *testing.T) {
	// Byte2 bits: tier0/high_bitdepth0/twelve_bit1/mono0/subsample11/position00.
	// twelve_bit must be0 when absent (profile0 and high_bitdepth0).
	if err := ValidateConfig([]byte{0x81, 0, 0x2c, 0}); err == nil {
		t.Fatal("impossible twelve_bit=1/high_bitdepth=0 configuration accepted")
	}
}
