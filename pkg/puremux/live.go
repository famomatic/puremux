package puremux

import (
	"io"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/webm"
	"github.com/famomatic/puremux/internal/preprocessor"
)

// Live elementary-stream ingestion helpers.
//
// These wrap Session.WritePacket for callers that receive raw elementary
// streams from a network source (rather than demuxing a container file):
// H.264/HEVC Annex-B access units and ADTS AAC frames, as delivered by e.g.
// RTMP/WebSocket live-streaming endpoints. Combined with
// Config.OutputContainer = ContainerMPEGTS this turns a Session into a live
// TS muxer whose preprocessor absorbs out-of-order and duplicated source
// timestamps (Enforcer) and enforces a keyframe-first start (Aligner).
//
// The payload is copied before it enters the pipeline: the Enforcer's jitter
// buffer holds packets across calls, so the caller may reuse its buffer
// immediately after the call returns.

// WriteVideo feeds one video access unit (Annex-B for H.264/HEVC) in DECODE
// order, stamped with t — a monotonic decode-order clock (the live delivery
// clock). t is used as the decode timestamp (DTS) directly.
//
// The presentation timestamp is derived from the BITSTREAM: for MPEG-TS
// output with H.264/HEVC, each AU's picture order count (POC) is parsed from
// the slice headers, and frames that the bitstream reorders (B-frames) get a
// PES PTS reflecting their true display slot on the decode clock (PES then
// carries a distinct PTS/DTS pair). Streams without B-frames collapse to
// PTS == DTS == t, byte-identical to earlier releases, with zero
// steady-state latency after a one-time startup probe of 8 pictures. For
// B-frame streams the added latency is a bounded reorder lookahead (the
// observed B-pyramid depth + margin, capped at the 16-frame DPB bound).
// Streams whose POC cannot be parsed (missing parameter sets, POC type 1)
// fall back to PTS == DTS per AU. Frames held by the probe/lookahead are
// drained by Close.
//
// The keyframe flag is derived from the track's codec detector so the
// Aligner can sync audio to the first IDR. Callers that already hold a
// correct distinct PTS/DTS pair can use WritePacket directly; callers whose
// per-frame timestamps are PRESENTATION times in decode order should use
// WriteVideoReordered instead. Do not mix the three on one track.
func (s *Session) WriteVideo(trackID int, au []byte, pts time.Duration) error {
	if s.closed {
		return io.ErrClosedPipe
	}
	idx, ok := s.trackByID[trackID]
	if !ok {
		return errUnknownTrack
	}
	spec := s.tracks[idx]
	p := core.AcquirePacket()
	p.Data = append(p.Data[:0], au...)
	p.PTS = pts
	p.DTS = pts
	p.Codec = spec.Codec
	p.TrackID = trackID
	p.IsKeyframe = s.detectors.Detector(spec.Codec).IsKeyframe(au)
	st := s.ptsStageFor(trackID, spec)
	if st == nil {
		return s.writePacket(p)
	}
	poc, isPic := st.parser.ParseAU(au)
	var werr error
	st.synth.Process(p, poc, isPic, func(out *core.Packet) {
		if werr == nil {
			werr = s.writePacket(out)
		} else {
			core.ReleasePacket(out)
		}
	})
	return werr
}

// ptsStageFor returns (lazily creating) the track's presentation stage, or
// nil when the stage does not apply: non-TS output (the WebM/MKV writer keys
// blocks on DTS only), non-video tracks, or codecs without bitstream display
// reordering.
func (s *Session) ptsStageFor(trackID int, spec webm.TrackSpec) *ptsStage {
	if !s.isTS() || !spec.IsVideo {
		return nil
	}
	if st, ok := s.ptsStages[trackID]; ok {
		return st
	}
	parser := core.NewPictureOrderParser(spec.Codec)
	if parser == nil {
		return nil
	}
	st := &ptsStage{
		parser: parser,
		synth:  preprocessor.NewPresentationSynthesizer(s.cfg.Preprocessor),
	}
	s.ptsStages[trackID] = st
	return st
}

// WriteVideoReordered feeds one video access unit (Annex-B for H.264/HEVC)
// in DECODE order, stamped with its per-frame PRESENTATION timestamp only.
// For streams with B-frame reordering such PTS are non-monotonic in decode
// order and cannot be used as DTS; this method synthesizes the DTS instead:
// payload (decode) order is preserved exactly, the synthesized decode
// timeline is strictly monotonic (advancing by at least
// Config.Preprocessor.MinMonotonicStep, so it survives quantization to the
// container clock), and DTS <= PTS holds per frame. The caller's PTS values
// are never altered.
//
// It is safe to call unconditionally, without knowing whether the stream has
// B-frames: input with monotonic timestamps collapses to the WriteVideo fast
// path (DTS == PTS, byte-identical output, zero steady-state latency).
// Added latency is bounded to a one-time startup probe of 8 frames (up to 32
// when the start is duplicate-heavy and carries no ordering evidence) while
// the stream's reorder depth is measured; afterwards every access unit is
// forwarded by the call that delivered it. The measurement is continuous, not
// locked at startup: it is order-tolerant within the probe window (a
// scrambled or duplicate-stamped backfill burst at session start yields a
// deterministic, valid timeline), and the decode delay keeps adapting
// afterwards — growing within a frame or two if a deeper B-pyramid appears
// (bridging frames may transiently carry DTS slightly above PTS before the
// timeline re-converges) and decaying back when a burst-inflated startup
// measurement exceeds the stream's real depth. Frames still held by the
// startup probe are drained by Close.
//
// Do not mix WriteVideo, WriteVideoReordered, and WritePacket on the same
// track: the synthesizer only sees frames fed through this method, so a
// bypassing packet would fork the decode timeline. Intended for
// ContainerMPEGTS output (PES carries the distinct PTS/DTS pair; the
// WebM/MKV writer keys blocks on DTS only).
func (s *Session) WriteVideoReordered(trackID int, au []byte, pts time.Duration) error {
	if s.closed {
		return io.ErrClosedPipe
	}
	idx, ok := s.trackByID[trackID]
	if !ok {
		return errUnknownTrack
	}
	spec := s.tracks[idx]
	sy := s.synths[trackID]
	if sy == nil {
		sy = preprocessor.NewDTSSynthesizer(s.cfg.Preprocessor)
		s.synths[trackID] = sy
	}
	p := core.AcquirePacket()
	p.Data = append(p.Data[:0], au...)
	p.PTS = pts
	p.DTS = pts
	p.Codec = spec.Codec
	p.TrackID = trackID
	p.IsKeyframe = s.detectors.Detector(spec.Codec).IsKeyframe(au)
	var werr error
	sy.Process(p, func(out *core.Packet) {
		if werr == nil {
			werr = s.writePacket(out)
		} else {
			core.ReleasePacket(out)
		}
	})
	return werr
}

// WriteADTS feeds a chunk of one or more concatenated ADTS AAC frames. The
// first frame is stamped with pts; each following frame advances by its own
// duration (samples / sample-rate from the ADTS header), so a multi-frame
// chunk yields correctly spaced per-frame packets. Bytes that do not parse
// as ADTS are skipped; a truncated trailing frame is ignored.
func (s *Session) WriteADTS(trackID int, chunk []byte, pts time.Duration) error {
	idx, ok := s.trackByID[trackID]
	if !ok {
		return errUnknownTrack
	}
	spec := s.tracks[idx]
	var werr error
	core.ForEachADTSFrame(chunk, func(frame []byte, info core.ADTSFrameInfo) bool {
		p := core.AcquirePacket()
		p.Data = append(p.Data[:0], frame...)
		p.PTS = pts
		p.DTS = pts
		p.Codec = spec.Codec
		p.TrackID = trackID
		if werr = s.WritePacket(p); werr != nil {
			return false
		}
		pts += info.Duration()
		return true
	})
	return werr
}
