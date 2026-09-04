package mp4

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestTimestampLessMatchesBigIntOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(0x4d5034))
	values := []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64}
	for range 2_000 {
		values = append(values, int64(rng.Uint64()))
	}
	for i, left := range values {
		right := int64(rng.Uint64())
		leftScale := rng.Uint32() | 1
		rightScale := rng.Uint32() | 1
		l := new(big.Int).Mul(big.NewInt(left), new(big.Int).SetUint64(uint64(rightScale)))
		r := new(big.Int).Mul(big.NewInt(right), new(big.Int).SetUint64(uint64(leftScale)))
		if got, want := timestampLess(left, leftScale, right, rightScale), l.Cmp(r) < 0; got != want {
			t.Fatalf("case %d: timestampLess(%d/%d, %d/%d) = %v, want %v", i, left, leftScale, right, rightScale, got, want)
		}
	}
}

func TestTimestampLessDoesNotAllocate(t *testing.T) {
	if got := testing.AllocsPerRun(1_000, func() {
		_ = timestampLess(math.MaxInt64, 90_000, math.MaxInt64-1, 48_000)
	}); got != 0 {
		t.Fatalf("timestampLess allocations = %v, want 0", got)
	}
}
