package preprocessor

import "github.com/famomatic/puremux/internal/core"

// Config bounds preprocessor state so jitter buffering cannot grow
// unbounded (ARCHITECTURE.md section 5.B).
type Config struct {
	// MaxBufferSize is the maximum number of packets held in the reorder
	// buffer before the overflow policy triggers. Must be > 0.
	MaxBufferSize int
	// MaxBufferDuration bounds buffer hold time. A packet older than this
	// relative to the newest seen is dropped even if the count limit has
	// not been hit. Zero disables the duration limit.
	MaxBufferDuration uint64
	// InterpolationGapThreshold is the largest timestamp gap (in
	// nanoseconds) the Enforcer classifies as a detectable discontinuity.
	// Gaps at or below this threshold are counted in DetectedGaps and
	// emitted as-is (no synthetic packets are fabricated). Larger gaps are
	// left as discontinuities without counting. Zero disables gap detection.
	InterpolationGapThreshold uint64
	// MinMonotonicStep is the minimum DTS advance (in nanoseconds) the
	// Enforcer forces between consecutive emitted packets on the same track.
	// Zero defaults to 1ns (strict monotonicity in the time.Duration domain).
	//
	// Set this to the output container's clock granularity when the container
	// uses a coarser clock than nanoseconds: e.g. MPEG-TS timestamps tick at
	// 90 kHz (~11.1us) and Matroska at the TimecodeScale (1ms default). A 1ns
	// nudge collapses back into a duplicate after quantization to those
	// clocks, so streams with duplicated source timestamps (common in live
	// backfill bursts) would still play back as "invalid timestamp" frames.
	MinMonotonicStep uint64
}

// DefaultConfig returns a conservative real-time-friendly configuration.
func DefaultConfig() Config {
	return Config{
		MaxBufferSize:             64,
		MaxBufferDuration:         400_000_000, // 400ms
		InterpolationGapThreshold: 100_000_000, // 100ms
	}
}

// Metrics reports observable preprocessor side effects so callers can
// detect stream degradation (ARCHITECTURE.md section 5.B overflow policy).
type Metrics struct {
	DroppedOverflow   uint64 // packets dropped due to buffer overflow
	DroppedOutOfOrder uint64 // packets dropped as too-late out-of-order
	// DetectedGaps counts timestamp gaps within the interpolatable range.
	// The Enforcer does NOT synthesize replacement packets (no decoder, no
	// sample counts); it only records that a gap was seen so callers can
	// detect the discontinuity. The former name InterpolatedGaps was
	// misleading because no interpolation actually occurs.
	DetectedGaps        uint64
	AudioPacketsDropped uint64 // whole audio packets dropped by the Aligner
	AudioPacketsDuped   uint64 // whole audio packets duplicated by the Aligner
}

// Preprocessor corrects a single in-memory packet stream. It NEVER writes
// to files (ARCHITECTURE.md section 5.B). Each track owns its own instance;
// there is no cross-track shared mutable state above the muxer merge step
// (ARCHITECTURE.md section 6).
type Preprocessor interface {
	// Process takes one inbound packet and emits zero or more corrected
	// packets via the callback. The caller retains ownership of inbound;
	// emitted packets are pool-acquired and must be released by the
	// receiver.
	Process(inbound *core.Packet, emit func(*core.Packet)) error
	// Metrics returns a snapshot of observable side effects.
	Metrics() Metrics
	// Reset clears all internal state for reuse on a fresh stream.
	Reset()
}
