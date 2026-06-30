package core

// CodecKeyframeDetector inspects a compressed payload's packet header bytes
// to report whether the packet holds a sync (key) frame.
//
// Implementations MUST read header bytes only and MUST NOT decode the
// payload to pixels/PCM. Each target codec (VP8, VP9, AV1) provides its own
// implementation; Opus has no keyframe concept and uses the no-op detector.
//
// This interface is the single point where codec-specific byte probing is
// permitted (see ARCHITECTURE.md section 4 and section 5.A). Preprocessor
// and muxer code MUST NOT inline codec byte probing; they call through here.
type CodecKeyframeDetector interface {
	// IsKeyframe inspects the packet header and reports sync-frame status.
	IsKeyframe(data []byte) bool
}

// noopDetector never reports a keyframe. Used by Opus (audio has no IDR).
type noopDetector struct{}

func (noopDetector) IsKeyframe([]byte) bool { return false }

// DetectorRegistry maps a CodecType to its keyframe detector. It is the only
// sanctioned lookup path for codec-specific header inspection.
type DetectorRegistry struct {
	detectors map[CodecType]CodecKeyframeDetector
}

// NewDetectorRegistry builds a registry pre-populated with the built-in
// codec detectors. Custom detectors may be registered via Register.
func NewDetectorRegistry() *DetectorRegistry {
	r := &DetectorRegistry{detectors: make(map[CodecType]CodecKeyframeDetector, 6)}
	r.Register(CodecVP8, vp8Detector{})
	r.Register(CodecVP9, vp9Detector{})
	r.Register(CodecAV1, av1Detector{})
	r.Register(CodecOpus, noopDetector{})
	return r
}

// Register associates a codec with a detector. Calling with a nil detector
// clears the mapping for that codec.
func (r *DetectorRegistry) Register(c CodecType, d CodecKeyframeDetector) {
	if d == nil {
		delete(r.detectors, c)
		return
	}
	r.detectors[c] = d
}

// Detector returns the detector for the codec, or a noopDetector if none is
// registered. The returned detector is safe to call on empty/nil data.
func (r *DetectorRegistry) Detector(c CodecType) CodecKeyframeDetector {
	if d, ok := r.detectors[c]; ok {
		return d
	}
	return noopDetector{}
}