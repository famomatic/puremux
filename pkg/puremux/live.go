package puremux

import (
	"time"

	"github.com/famomatic/puremux/internal/core"
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

// WriteVideo feeds one video access unit (Annex-B for H.264/HEVC) stamped
// with pts (== dts; deliver streams with B-frame reordering via WritePacket
// with distinct PTS/DTS instead). The keyframe flag is derived from the
// track's codec detector so the Aligner can sync audio to the first IDR.
func (s *Session) WriteVideo(trackID int, au []byte, pts time.Duration) error {
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
	return s.WritePacket(p)
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
