package preprocessor

import (
	"time"

	"github.com/famomatic/puremux/internal/core"
)

// Aligner enforces that a video stream starts with a keyframe and realigns
// the audio track to match. It NEVER writes to a file (ARCHITECTURE.md
// section 5.B). Audio realignment is packet-granular only: it may drop or
// duplicate whole Opus packets but MUST NOT trim within a packet, because
// sample counts are unknown under the opacity rule (§4) and trimming would
// require decoding.
//
// The Aligner depends on a CodecKeyframeDetector; it never probes codec bytes
// directly (§5.A).
type Aligner struct {
	detector core.CodecKeyframeDetector
	video    bool
	syncStart time.Duration // DTS of the first accepted keyframe
	started   bool
	metrics   Metrics
}

// NewAligner builds an Aligner for a track of the given kind. video tracks
// require a non-nil detector; audio tracks pass a noop detector.
func NewAligner(detector core.CodecKeyframeDetector, isVideo bool) *Aligner {
	return &Aligner{detector: detector, video: isVideo}
}

// Process applies alignment rules to inbound.
//
// Video: packets before the first keyframe are dropped (not emitted). Once a
// keyframe is seen, all subsequent packets pass through unchanged. The DTS of
// the first keyframe becomes the sync start.
//
// Audio: once the video syncStart is known (via SetVideoSyncStart), packets
// with DTS < syncStart are dropped; packets at or after syncStart pass
// through. We do not fabricate or trim packets.
func (a *Aligner) Process(inbound *core.Packet, emit func(*core.Packet)) error {
	if inbound == nil {
		return nil
	}
	if a.video {
		return a.processVideo(inbound, emit)
	}
	return a.processAudio(inbound, emit)
}

func (a *Aligner) processVideo(inbound *core.Packet, emit func(*core.Packet)) error {
	if !a.started {
		// Use the detector (never inline byte probing, §5.A).
		if !a.detector.IsKeyframe(inbound.Data) && !inbound.IsKeyframe {
			// Drop packets before the first IDR.
			a.metrics.DroppedOutOfOrder++
			core.ReleasePacket(inbound)
			return nil
		}
		a.started = true
		a.syncStart = inbound.DTS
	}
	emit(inbound)
	return nil
}

func (a *Aligner) processAudio(inbound *core.Packet, emit func(*core.Packet)) error {
	if !a.started {
		// Audio alignment waits for the video sync start to be set.
		// If unset, pass through (audio-only stream).
		emit(inbound)
		return nil
	}
	// Packet-granular drop only: drop whole packets before syncStart.
	// We MUST NOT trim within a packet (§5.B).
	if inbound.DTS < a.syncStart {
		a.metrics.AudioPacketsDropped++
		core.ReleasePacket(inbound)
		return nil
	}
	emit(inbound)
	return nil
}

// SetVideoSyncStart propagates the video keyframe DTS so the audio aligner
// can drop audio packets that precede the video sync point. Called by the
// pipeline coordinator once the video aligner locks its sync start.
func (a *Aligner) SetVideoSyncStart(start time.Duration) {
	a.syncStart = start
	a.started = true
}

// Metrics returns a snapshot of observable side effects.
func (a *Aligner) Metrics() Metrics { return a.metrics }

// Reset clears all internal state for reuse on a fresh stream.
func (a *Aligner) Reset() {
	a.started = false
	a.syncStart = 0
	a.metrics = Metrics{}
}
