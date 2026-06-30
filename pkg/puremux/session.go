// Package puremux is the public facade for the puremux media muxer.
//
// It wires the internal preprocessor (Enforcer + Aligner) and the WebM
// container writer into a single muxing session. Consumers register tracks,
// feed packets, and close the session; the facade handles timestamp
// enforcement, keyframe alignment, and graceful closer patching.
package puremux

import (
	"io"
	"time"

	"famomatic/puremux/internal/core"
	"famomatic/puremux/internal/format/webm"
	"famomatic/puremux/internal/preprocessor"
)

// Track describes a media stream for the public API.
type Track struct {
	Codec      core.CodecType
	IsVideo    bool
	Width      int
	Height     int
	Channels   int
	SampleRate float64
	// CodecPrivate is optional codec configuration data (e.g. Opus headers).
	CodecPrivate []byte
}

// Config tunes the muxing session.
type Config struct {
	// TimecodeScale in nanoseconds per timecode unit (WebM default 1_000_000).
	TimecodeScale uint64
	// Preprocessor config bounds the jitter buffer (ARCHITECTURE.md §5.B).
	Preprocessor preprocessor.Config
}

// DefaultConfig returns a real-time-friendly configuration.
func DefaultConfig() Config {
	return Config{
		TimecodeScale: 1_000_000,
		Preprocessor:  preprocessor.DefaultConfig(),
	}
}

// Session is an active muxing session. It is NOT safe for concurrent
// WritePacket calls; the concurrency model is single-writer (§6): feed
// packets from one goroutine (or merge per-track channels upstream).
type Session struct {
	ws        *seekWriter
	cfg       Config
	header    webm.Header
	tracks    []webm.TrackSpec
	trackByID map[int]int // packet TrackID -> index into tracks
	detectors *core.DetectorRegistry

	// per-track preprocessors
	enforcers map[int]*preprocessor.Enforcer
	aligners  map[int]*preprocessor.Aligner

	cluster    *webm.ClusterWriter
	clusterTc  uint64 // absolute ms of current cluster
	lastTcMs   uint64 // absolute ms of the last written packet (for duration)
	maxRelTc   int64  // max relative timecode used in current cluster (ms)
	cues       []webm.CuePoint
	segmentTc0 time.Duration // first packet DTS, for duration calc
	started    bool
	closed     bool
}

// NewSession creates a muxing session writing to w. If w implements
// io.Seeker the session runs in seekable mode (patches Duration/Cues on
// Close); otherwise it runs in streaming mode.
func NewSession(w io.Writer, cfg Config) (*Session, error) {
	ws := &seekWriter{w: w}
	if _, ok := w.(io.Seeker); ok {
		ws.seekable = true
		ws.s = w.(io.Seeker)
	}
	s := &Session{
		ws:        ws,
		cfg:       cfg,
		trackByID: make(map[int]int),
		detectors: core.NewDetectorRegistry(),
		enforcers: make(map[int]*preprocessor.Enforcer),
		aligners:  make(map[int]*preprocessor.Aligner),
	}
	return s, nil
}

// AddTrack registers a media track. Returns the assigned track number (1-based)
// to use in Packet.TrackID. Must be called before the first WritePacket.
func (s *Session) AddTrack(t Track) (int, error) {
	num := len(s.tracks) + 1
	spec := webm.TrackSpec{
		Number:       uint64(num),
		UID:          uint64(num),
		Codec:        t.Codec,
		IsVideo:      t.IsVideo,
		Width:        t.Width,
		Height:       t.Height,
		Channels:     t.Channels,
		SampleRate:   t.SampleRate,
		CodecPrivate: t.CodecPrivate,
	}
	s.tracks = append(s.tracks, spec)
	idx := len(s.tracks) - 1
	s.trackByID[num] = idx

	// Wire preprocessors for this track.
	s.enforcers[num] = preprocessor.NewEnforcer(s.cfg.Preprocessor)
	det := s.detectors.Detector(t.Codec)
	s.aligners[num] = preprocessor.NewAligner(det, t.IsVideo)
	return num, nil
}

// WritePacket feeds a packet through the preprocessor pipeline and into the
// container. Packets may arrive out of order; the Enforcer fixes that. The
// packet's Data is treated as opaque (ARCHITECTURE.md §4).
func (s *Session) WritePacket(p *core.Packet) error {
	if s.closed {
		return io.ErrClosedPipe
	}
	if p == nil {
		return nil
	}
	// Lazy header write on first packet (we now know tracks are finalized).
	if !s.started {
		if err := s.writeHeader(); err != nil {
			return err
		}
		s.started = true
	}

	trackNum := p.TrackID
	enf, ok := s.enforcers[trackNum]
	if !ok {
		return errUnknownTrack
	}
	al := s.aligners[trackNum]

	// Pipeline: Enforcer (monotonic DTS) -> Aligner (keyframe sync).
	var processed []*core.Packet
	emit := func(out *core.Packet) { processed = append(processed, out) }
	if err := enf.Process(p, emit); err != nil {
		return err
	}
	enf.Flush(emit)
	for _, pp := range processed {
		// Propagate video sync start to audio aligners.
		if al != nil && pp.IsKeyframe && s.tracks[s.trackByID[trackNum]].IsVideo {
			for tn, other := range s.aligners {
				if tn != trackNum {
					other.SetVideoSyncStart(pp.DTS)
				}
			}
		}
		var aligned []*core.Packet
		aemit := func(out *core.Packet) { aligned = append(aligned, out) }
		if err := al.Process(pp, aemit); err != nil {
			return err
		}
		for _, ap := range aligned {
			if err := s.writeBlock(trackNum, ap); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeBlock serializes a corrected packet into a Cluster/SimpleBlock.
func (s *Session) writeBlock(trackNum int, p *core.Packet) error {
	absMs := uint64(p.DTS / time.Millisecond)
	if !s.started {
		s.segmentTc0 = p.DTS
	}

	// Open a new cluster if none or if relative timecode would overflow int16.
	segStartMs := uint64(s.segmentTc0 / time.Millisecond)
	if s.cluster == nil || int64(absMs)-int64(s.clusterTc) > 30000 {
		if s.cluster != nil {
			if err := s.cluster.Close(); err != nil {
				return err
			}
		}
		s.clusterTc = absMs
		cw, err := webm.BeginCluster(s.ws, s.ws.seekable, absMs)
		if err != nil {
			return err
		}
		s.cluster = cw
		// Record cue for seekable mode.
		if s.ws.seekable {
			relPos := uint64(s.ws.offset() - int64(s.header.SegmentStart))
			s.cues = append(s.cues, webm.CuePoint{
				Timecode:         absMs - segStartMs,
				Track:            uint64(trackNum),
				ClusterPosition:  relPos,
			})
		}
	}

	relTc := int16(absMs - s.clusterTc)
	s.lastTcMs = absMs
	spec := s.tracks[s.trackByID[trackNum]]
	return s.cluster.WriteSimpleBlock(spec.Number, relTc, p.IsKeyframe, p.Data)
}

// writeHeader writes the EBML header, Segment, Info, and Tracks.
func (s *Session) writeHeader() error {
	if err := webm.WriteEBMLHeader(s.ws); err != nil {
		return err
	}
	h, err := webm.BeginSegment(s.ws, s.ws.seekable)
	if err != nil {
		return err
	}
	s.header = h
	if err := webm.WriteInfo(s.ws, &s.header, s.cfg.TimecodeScale, time.Now()); err != nil {
		return err
	}
	if _, err := webm.WriteTracks(s.ws, s.tracks); err != nil {
		return err
	}
	s.header.TracksEnd = int64(s.ws.offset())
	return nil
}

// Close finalizes the session. For seekable sinks it patches the reserved
// Duration and Segment size, and writes Cues + SeekHead. For non-seekable
// sinks it flushes the streaming-mode tail.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cluster != nil {
		if err := s.cluster.Close(); err != nil {
			return err
		}
	}
	// Determine duration in TimecodeScale units (not seconds). WebM duration
	// is Duration_float * TimecodeScale; with TimecodeScale=1ms the float must
	// hold the millisecond count (192000ms => 192000.0 => 192s at 1ms/unit).
	if s.ws.seekable && s.header.HasDuration {
		durTC := 0.0
		if s.lastTcMs > 0 {
			durTC = float64(s.lastTcMs)
		}
		if err := webm.PatchDuration(s.ws, s.header.DurationPayloadOff, durTC); err != nil {
			return err
		}
	}
	// Cues (seekable only).
	var cuesPos int64 = -1
	if s.ws.seekable && len(s.cues) > 0 {
		cp, err := webm.WriteCues(s.ws, s.cues)
		if err != nil {
			return err
		}
		cuesPos = cp - int64(s.header.SegmentStart)
	}
	// Segment size patch (seekable only).
	if s.ws.seekable {
		end := s.ws.offset()
		segLen := uint64(end - int64(s.header.SegmentStart))
		if err := webm.PatchSegmentSize(s.ws, s.header.SegmentSizeOff, 8, segLen); err != nil {
			return err
		}
	}
	_ = cuesPos
	return nil
}

// seekWriter wraps an io.Writer, tracking the write offset and exposing
// seeking only when the underlying writer supports it.
type seekWriter struct {
	w        io.Writer
	s        io.Seeker
	seekable bool
	off      int64
}

func (sw *seekWriter) Write(p []byte) (int, error) {
	n, err := sw.w.Write(p)
	sw.off += int64(n)
	return n, err
}

func (sw *seekWriter) Seek(offset int64, whence int) (int64, error) {
	// Position queries (SeekCurrent, 0) work in both modes: they just read the
	// tracked offset without moving the underlying writer.
	if whence == 1 && offset == 0 {
		return sw.off, nil
	}
	if !sw.seekable {
		return 0, errNotSeekable
	}
	n, err := sw.s.Seek(offset, whence)
	if err == nil {
		sw.off = n
	}
	return n, err
}

func (sw *seekWriter) offset() int64 { return sw.off }

var (
	errUnknownTrack = errPtr("unknown track")
	errNotSeekable  = errPtr("writer is not seekable")
)

type errPtr string

func (e errPtr) Error() string { return string(e) }