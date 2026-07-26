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
	detector  core.CodecKeyframeDetector
	video     bool
	syncStart time.Duration // DTS of the first accepted keyframe
	started   bool
	// expectsVideoSync is true for audio tracks in a session that also has a
	// video track. Such audio is held until SetVideoSyncStart arrives, so
	// packets before the video sync point are dropped instead of leaking out.
	expectsVideoSync bool
	// pending holds audio packets that arrived before the video sync start
	// was locked. Bounded by maxPending; overflow drops the oldest.
	pending    []*core.Packet
	maxPending int
	// cfgPending holds pre-keyframe VIDEO packets that carry only decoder
	// configuration (SPS/PPS parameter sets, per CodecConfigOnlyDetector).
	// They are emitted ahead of the first keyframe instead of being dropped:
	// if the sync frame does not repeat the parameter sets in-band, dropping
	// them would leave the whole stream undecodable. Bounded; overflow drops
	// the oldest (the newest configuration is the one that matters).
	cfgPending []*core.Packet
	cfgOnly    core.CodecConfigOnlyDetector // nil when the codec has no notion
	metrics    Metrics
}

// NewAligner builds an Aligner for a track of the given kind. video tracks
// require a non-nil detector; audio tracks pass a noop detector.
func NewAligner(detector core.CodecKeyframeDetector, isVideo bool) *Aligner {
	a := &Aligner{detector: detector, video: isVideo}
	if cfg, ok := detector.(core.CodecConfigOnlyDetector); ok {
		a.cfgOnly = cfg
	}
	return a
}

// maxCfgPending bounds the held configuration packets; parameter sets are a
// handful of small NALs, so a short queue holds every realistic burst.
const maxCfgPending = 8

// NewAlignerForSession builds an Aligner that knows whether the session also
// contains a video track. When expectsVideoSync is true, audio packets are
// held in a bounded pending queue (64 packets; overflow drops the oldest)
// until SetVideoSyncStart arrives rather than being emitted before the video
// sync point.
func NewAlignerForSession(detector core.CodecKeyframeDetector, isVideo, expectsVideoSync bool) *Aligner {
	a := NewAligner(detector, isVideo)
	a.expectsVideoSync = expectsVideoSync
	a.maxPending = 64
	return a
}

// SetExpectsVideoSync configures whether this audio aligner should hold
// packets until the video sync start arrives. The session sets this to false
// for audio-only streams once all tracks are registered, so audio is not held
// indefinitely. If switched from true to false, any held pending packets are
// flushed via emit.
func (a *Aligner) SetExpectsVideoSync(expect bool, emit func(*core.Packet)) {
	if a.video {
		return // video aligners are unaffected
	}
	if a.expectsVideoSync && !expect {
		// No video sync will arrive; release held packets in order.
		a.drainPendingPassthrough(emit)
	}
	a.expectsVideoSync = expect
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
			// A configuration-only packet (standalone SPS/PPS) is decoder
			// state, not a frame: hold it for the sync point instead of
			// dropping it with the undecodable pre-keyframe pictures.
			if a.cfgOnly != nil && a.cfgOnly.IsConfigOnly(inbound.Data) {
				a.cfgPending = append(a.cfgPending, inbound)
				if len(a.cfgPending) > maxCfgPending {
					dropped := a.cfgPending[0]
					copy(a.cfgPending, a.cfgPending[1:])
					a.cfgPending[len(a.cfgPending)-1] = nil
					a.cfgPending = a.cfgPending[:len(a.cfgPending)-1]
					a.metrics.DroppedOutOfOrder++
					core.ReleasePacket(dropped)
				}
				return nil
			}
			// Drop packets before the first IDR.
			a.metrics.DroppedOutOfOrder++
			core.ReleasePacket(inbound)
			return nil
		}
		a.started = true
		a.syncStart = inbound.DTS
		// Replay the held configuration ahead of the first keyframe so the
		// decoder is initialized even when the keyframe does not repeat the
		// parameter sets in-band.
		for i, cfg := range a.cfgPending {
			emit(cfg)
			a.cfgPending[i] = nil
		}
		a.cfgPending = a.cfgPending[:0]
	}
	emit(inbound)
	return nil
}

func (a *Aligner) processAudio(inbound *core.Packet, emit func(*core.Packet)) error {
	if !a.started {
		if a.expectsVideoSync {
			// Hold this packet until the video sync start arrives. Bounded
			// so a missing keyframe cannot grow the queue unboundedly.
			a.pending = append(a.pending, inbound)
			if len(a.pending) > a.maxPending {
				dropped := a.pending[0]
				copy(a.pending, a.pending[1:])
				a.pending[len(a.pending)-1] = nil
				a.pending = a.pending[:len(a.pending)-1]
				a.metrics.AudioPacketsDropped++
				core.ReleasePacket(dropped)
			}
			return nil
		}
		// Audio-only stream: no video sync to wait for, pass through.
		emit(inbound)
		return nil
	}
	a.drainPending(emit)
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

// drainPending emits or drops the held audio packets relative to syncStart.
func (a *Aligner) drainPending(emit func(*core.Packet)) {
	for _, p := range a.pending {
		if p.DTS < a.syncStart {
			a.metrics.AudioPacketsDropped++
			core.ReleasePacket(p)
			continue
		}
		emit(p)
	}
	// Clear references so the packets are not retained by the backing array.
	for i := range a.pending {
		a.pending[i] = nil
	}
	a.pending = a.pending[:0]
}

// drainPendingPassthrough releases held packets without sync filtering, used
// when the session turns out to be audio-only.
func (a *Aligner) drainPendingPassthrough(emit func(*core.Packet)) {
	for _, p := range a.pending {
		emit(p)
	}
	for i := range a.pending {
		a.pending[i] = nil
	}
	a.pending = a.pending[:0]
	a.started = true // mark started so audio passes through directly now
}

// SetVideoSyncStart propagates the video keyframe DTS so the audio aligner
// can drop audio packets that precede the video sync point. Called by the
// pipeline coordinator once the video aligner locks its sync start.
//
// It latches exactly once and only affects audio aligners:
//   - Video guard: without it, one video track's keyframe would force another
//     video aligner's started=true, making it emit mid-GOP (undecodable head).
//   - Idempotency: the coordinator calls this for every emitted video keyframe;
//     re-latching to a later keyframe's DTS would drop audio in [old, new),
//     chopping audio at each GOP boundary.
func (a *Aligner) SetVideoSyncStart(start time.Duration) {
	if a.video {
		return
	}
	if a.started {
		return
	}
	a.syncStart = start
	a.started = true
}

// Flush emits any audio packets still held in the pending queue. Called at
// end-of-stream so held audio is not lost. If a video sync start was locked,
// held packets are filtered against it; otherwise (no video keyframe ever
// arrived) they are released as passthrough rather than dropped.
func (a *Aligner) Flush(emit func(*core.Packet)) {
	// A video stream that ended without ever producing a keyframe leaves its
	// held configuration packets unusable; release them.
	for i, p := range a.cfgPending {
		core.ReleasePacket(p)
		a.cfgPending[i] = nil
	}
	a.cfgPending = a.cfgPending[:0]
	if len(a.pending) == 0 {
		return
	}
	if a.started {
		a.drainPending(emit)
	} else {
		a.drainPendingPassthrough(emit)
	}
}

// Metrics returns a snapshot of observable side effects.
func (a *Aligner) Metrics() Metrics { return a.metrics }

// Reset clears all internal state for reuse on a fresh stream.
func (a *Aligner) Reset() {
	for _, p := range a.pending {
		core.ReleasePacket(p)
	}
	a.pending = a.pending[:0]
	for i, p := range a.cfgPending {
		core.ReleasePacket(p)
		a.cfgPending[i] = nil
	}
	a.cfgPending = a.cfgPending[:0]
	a.started = false
	a.syncStart = 0
	a.metrics = Metrics{}
}
