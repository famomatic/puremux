// Package puremux is the public facade for the puremux media muxer.
//
// It wires the internal preprocessor (Enforcer + Aligner) and the WebM
// container writer into a single muxing session. Consumers register tracks,
// feed packets, and close the session; the facade handles timestamp
// enforcement, keyframe alignment, and graceful closer patching.
package puremux

import (
	"io"
	"slices"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/mpegts"
	"github.com/famomatic/puremux/internal/format/webm"
	"github.com/famomatic/puremux/internal/preprocessor"
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
	// OutputContainer selects the container written by the session. Zero
	// defaults to WebM for backward compatibility. Set to ContainerMKV to
	// emit a Matroska container (codec superset of WebM), or to
	// ContainerMPEGTS to emit a live MPEG-2 Transport Stream (H.264/HEVC
	// Annex-B video + ADTS AAC audio; see WriteVideo/WriteADTS).
	OutputContainer Container
}

// DefaultConfig returns a real-time-friendly configuration.
func DefaultConfig() Config {
	return Config{
		TimecodeScale:   1_000_000,
		Preprocessor:    preprocessor.DefaultConfig(),
		OutputContainer: ContainerWebM,
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
	// synths holds per-track DTS synthesizers, created lazily on the first
	// WriteVideoReordered call for a track. They sit BEFORE the Enforcer in
	// the pipeline and may hold frames across calls (startup probe), so Close
	// drains them first.
	synths map[int]*preprocessor.DTSSynthesizer
	// ptsStages holds the per-track presentation-timestamp pipelines for the
	// WriteVideo live path (MPEG-TS H.264/HEVC): a core POC parser plus a
	// preprocessor.PresentationSynthesizer that derives display-order PTS
	// from the bitstream picture order. Created lazily on the first
	// WriteVideo call for an eligible track; may hold frames across calls
	// (probe + reorder lookahead), so Close drains them like synths.
	ptsStages map[int]*ptsStage

	cluster    *webm.ClusterWriter
	clusterTc  uint64 // absolute ms of current cluster
	lastTcMs   uint64 // absolute ms of the last written packet (for duration)
	maxRelTc   int64  // max relative timecode used in current cluster (ms)
	cues       []webm.CuePoint
	segmentTc0 time.Duration // first emitted packet DTS; cue timecodes are relative to this
	segTc0Set  bool          // true once segmentTc0 has been captured from the first emitted packet
	started    bool
	closed     bool

	// tsMux is the MPEG-TS backend, non-nil only when
	// Config.OutputContainer == ContainerMPEGTS. The EBML fields above are
	// unused in that mode; the shared preprocessor pipeline feeds tsMux.
	tsMux *mpegts.Muxer
}

// isTS reports whether the session writes an MPEG-TS container.
func (s *Session) isTS() bool { return s.cfg.OutputContainer == ContainerMPEGTS }

// NewSession creates a muxing session writing to w. If w implements
// io.Seeker the session runs in seekable mode (patches Duration/Cues on
// Close); otherwise it runs in streaming mode.
func NewSession(w io.Writer, cfg Config) (*Session, error) {
	ws := &seekWriter{w: w}
	if _, ok := w.(io.Seeker); ok {
		ws.seekable = true
		ws.s = w.(io.Seeker)
	}
	// The block writer emits timecodes in milliseconds, which only agrees with
	// the Info TimecodeScale when the scale is 1ms (1_000_000 ns). A caller who
	// builds Config{} directly (bypassing DefaultConfig) leaves it 0, which is
	// an invalid WebM scale; normalize it here so the header and blocks agree.
	if cfg.TimecodeScale == 0 {
		cfg.TimecodeScale = 1_000_000
	}
	// MPEG-TS PES timestamps are quantized to the 90 kHz clock (one tick =
	// ceil(1e9/90000) = 11112 ns). The monotonic-DTS nudge advances by
	// MinMonotonicStep nanoseconds; a value below one tick rounds to zero and
	// leaves duplicate PES DTS ("non monotonically increasing dts"). Ensure the
	// nudge is at least one tick for TS output so a caller using DefaultConfig()
	// (which leaves MinMonotonicStep 0 → a 1 ns default) still gets strictly
	// monotonic DTS.
	if cfg.OutputContainer == ContainerMPEGTS {
		const tsTickNs uint64 = (uint64(time.Second) + 90000 - 1) / 90000
		if cfg.Preprocessor.MinMonotonicStep < tsTickNs {
			cfg.Preprocessor.MinMonotonicStep = tsTickNs
		}
	}
	s := &Session{
		ws:        ws,
		cfg:       cfg,
		trackByID: make(map[int]int),
		detectors: core.NewDetectorRegistry(),
		enforcers: make(map[int]*preprocessor.Enforcer),
		aligners:  make(map[int]*preprocessor.Aligner),
		synths:    make(map[int]*preprocessor.DTSSynthesizer),
		ptsStages: make(map[int]*ptsStage),
	}
	return s, nil
}

// ptsStage pairs a codec picture-order parser with the presentation
// synthesizer it feeds (one per WriteVideo track). The parser is the only
// place the AU bytes are inspected (§5.A); the synthesizer works on
// timestamps and POCInfo alone.
type ptsStage struct {
	parser core.PictureOrderParser
	synth  *preprocessor.PresentationSynthesizer
}

// AddTrack registers a media track. Returns the assigned track number (1-based)
// to use in Packet.TrackID. Must be called before the first WritePacket.
func (s *Session) AddTrack(t Track) (int, error) {
	// Tracks are serialized into the header on the first WritePacket; adding one
	// afterward would produce SimpleBlocks referencing a track number absent
	// from the header (an invalid file).
	if s.started {
		return 0, errTrackAfterStart
	}
	out := s.cfg.OutputContainer
	if out == ContainerUnknown {
		out = ContainerWebM
	}
	if !codecAllowed(out, t.Codec) {
		return 0, errUnsupportedCodec
	}
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
	// Audio tracks default to expecting a video sync; writeHeader re-evaluates
	// once all tracks are known and clears the flag for audio-only sessions.
	s.aligners[num] = preprocessor.NewAlignerForSession(det, t.IsVideo, !t.IsVideo)
	return num, nil
}

// WritePacket feeds a packet through the preprocessor pipeline and into the
// container. Packets may arrive out of order; the Enforcer fixes that. The
// packet's Data is treated as opaque (ARCHITECTURE.md §4).
func (s *Session) WritePacket(p *core.Packet) error {
	if s.closed {
		return io.ErrClosedPipe
	}
	return s.writePacket(p)
}

// writePacket is WritePacket without the closed-session guard, so Close can
// drain the DTS synthesizers' held frames into the pipeline after latching
// the closed flag.
func (s *Session) writePacket(p *core.Packet) error {
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

	// Pipeline: Enforcer (monotonic DTS, jitter reorder) -> Aligner (keyframe
	// sync). The Enforcer is NOT flushed per packet: its reorder window holds
	// slightly-out-of-order packets so a late successor can still slot ahead.
	// Held packets are committed by the flush in Close. (The old per-packet
	// Flush drained the whole buffer every call, defeating reordering and
	// causing any out-of-order packet to be dropped instead of reordered.)
	var processed []*core.Packet
	emit := func(out *core.Packet) { processed = append(processed, out) }
	if err := enf.Process(p, emit); err != nil {
		return err
	}
	for _, pp := range processed {
		if err := s.pushThroughAligner(trackNum, pp); err != nil {
			return err
		}
	}
	return nil
}

// pushThroughAligner runs one Enforcer-emitted packet through the track's
// Aligner and writes the aligned output as SimpleBlocks. Emitted packets are
// released back to the pool after being copied into the container.
func (s *Session) pushThroughAligner(trackNum int, pp *core.Packet) error {
	al := s.aligners[trackNum]
	// Propagate video sync start to the (audio) aligners of other tracks. The
	// SetVideoSyncStart guards make this a no-op for other video tracks and
	// idempotent after the first keyframe.
	if al != nil && pp.IsKeyframe && s.tracks[s.trackByID[trackNum]].IsVideo {
		for tn, other := range s.aligners {
			if tn != trackNum {
				other.SetVideoSyncStart(pp.DTS)
			}
		}
	}
	var aligned []*core.Packet
	aemit := func(out *core.Packet) { aligned = append(aligned, out) }
	if al != nil {
		if err := al.Process(pp, aemit); err != nil {
			return err
		}
	} else {
		aligned = append(aligned, pp)
	}
	for _, ap := range aligned {
		if err := s.writeBlock(trackNum, ap); err != nil {
			return err
		}
		// writeBlock copies the payload into the container synchronously, so the
		// pool-acquired packet is now safe to return.
		core.ReleasePacket(ap)
	}
	return nil
}

// writeBlock serializes a corrected packet into the container: a
// Cluster/SimpleBlock for WebM/MKV, or a PES packet for MPEG-TS.
func (s *Session) writeBlock(trackNum int, p *core.Packet) error {
	if s.isTS() {
		// The TS muxer rebases timestamps internally (first packet + headroom
		// offset); no EBML cluster/cue bookkeeping applies.
		return s.tsMux.WritePacket(p)
	}
	absMs := uint64(p.DTS / time.Millisecond)
	// Capture the segment origin from the first packet actually written.
	if !s.segTc0Set {
		s.segmentTc0 = p.DTS
		s.segTc0Set = true
	}

	// Normalize the output timeline to start at 0 (the first written packet).
	// Packets are monotonic post-Enforcer, so absMs >= segStartMs; clamp to be
	// safe. Both the Cluster Timestamp and the Cue time use this normalized
	// value so they agree (a prerequisite for correct seeking).
	segStartMs := uint64(s.segmentTc0 / time.Millisecond)
	normMs := uint64(0)
	if absMs > segStartMs {
		normMs = absMs - segStartMs
	}

	// Open a new cluster if none, or if the relative timecode would overflow
	// the SimpleBlock int16 range. Use signed math: normMs may precede the
	// cluster timecode after Enforcer reordering.
	relFromCluster := int64(normMs) - int64(s.clusterTc)
	if s.cluster == nil || relFromCluster > 30000 || relFromCluster < -32768 {
		if s.cluster != nil {
			if err := s.cluster.Close(); err != nil {
				return err
			}
		}
		s.clusterTc = normMs
		cw, err := webm.BeginCluster(s.ws, s.ws.seekable, normMs)
		if err != nil {
			return err
		}
		s.cluster = cw
		// Record cue for seekable mode. ClusterPosition must point at the
		// Cluster element start (captured before its header was written), not
		// at the post-header offset.
		if s.ws.seekable {
			relPos := uint64(cw.StartOffset() - int64(s.header.SegmentStart))
			s.cues = append(s.cues, webm.CuePoint{
				Timecode:        normMs,
				Track:           uint64(trackNum),
				ClusterPosition: relPos,
			})
		}
		// Recompute now that the cluster timecode is normMs (relTc = 0 for the
		// cluster's first block). The old code left relFromCluster stale, so the
		// first block of every new cluster carried the pre-rollover relative
		// timecode (e.g. ~30001), roughly doubling its absolute time.
		relFromCluster = int64(normMs) - int64(s.clusterTc)
	}

	relTc := int16(relFromCluster)
	s.lastTcMs = normMs
	spec := s.tracks[s.trackByID[trackNum]]
	return s.cluster.WriteSimpleBlock(spec.Number, relTc, p.IsKeyframe, p.Data)
}

// writeHeader initializes the container: EBML header + Segment + Info +
// Tracks for WebM/MKV, or the TS muxer track table for MPEG-TS (whose PAT/PMT
// are emitted lazily with the first packet).
func (s *Session) writeHeader() error {
	if s.isTS() {
		s.tsMux = mpegts.New(s.ws)
		for _, spec := range s.tracks {
			kind := core.TrackAudio
			if spec.IsVideo {
				kind = core.TrackVideo
			}
			if _, err := s.tsMux.AddTrack(core.Track{
				ID:         int(spec.Number),
				Kind:       kind,
				Codec:      spec.Codec,
				Width:      spec.Width,
				Height:     spec.Height,
				Channels:   spec.Channels,
				SampleRate: int(spec.SampleRate),
			}); err != nil {
				return err
			}
		}
		s.resolveAudioSync()
		return nil
	}
	doctype := webm.DocTypeWebM
	if s.cfg.OutputContainer == ContainerMKV {
		doctype = webm.DocTypeMatroska
	}
	if err := webm.WriteEBMLHeaderFor(s.ws, doctype); err != nil {
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
	s.header.TracksStart = int64(s.ws.offset())
	if _, err := webm.WriteTracks(s.ws, s.tracks); err != nil {
		return err
	}
	s.header.TracksEnd = int64(s.ws.offset())
	s.resolveAudioSync()
	return nil
}

// resolveAudioSync decides, once all tracks are registered, whether audio
// aligners should hold for a video sync start. An audio-only session has no
// video track, so holding would never resolve. The first packet has not been
// processed yet (writeHeader runs before the pipeline), so pending queues are
// empty.
func (s *Session) resolveAudioSync() {
	for _, spec := range s.tracks {
		if spec.IsVideo {
			return
		}
	}
	noemit := func(*core.Packet) {}
	for _, al := range s.aligners {
		al.SetExpectsVideoSync(false, noemit)
	}
}

// Close finalizes the session. For seekable sinks it patches the reserved
// Duration and Segment size, and writes Cues + SeekHead. For non-seekable
// sinks it flushes the streaming-mode tail.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// Drain the per-track presentation stages and DTS synthesizers first: a
	// stream shorter than a startup probe still holds every frame in them
	// (in which case even the header is unwritten — writePacket below writes
	// it). The track iteration is ordered for deterministic multi-track
	// output.
	stageTracks := make([]int, 0, len(s.ptsStages))
	for tn := range s.ptsStages {
		stageTracks = append(stageTracks, tn)
	}
	slices.Sort(stageTracks)
	for _, tn := range stageTracks {
		var werr error
		s.ptsStages[tn].synth.Flush(func(out *core.Packet) {
			if werr == nil {
				werr = s.writePacket(out)
			} else {
				core.ReleasePacket(out)
			}
		})
		if werr != nil {
			return werr
		}
	}
	synthTracks := make([]int, 0, len(s.synths))
	for tn := range s.synths {
		synthTracks = append(synthTracks, tn)
	}
	slices.Sort(synthTracks)
	for _, tn := range synthTracks {
		var werr error
		s.synths[tn].Flush(func(out *core.Packet) {
			if werr == nil {
				werr = s.writePacket(out)
			} else {
				core.ReleasePacket(out)
			}
		})
		if werr != nil {
			return werr
		}
	}
	// If no packet was ever written, no header/Segment exists. Finalizing would
	// write a stray SeekHead and patch a non-existent Segment size at offset 0,
	// producing a malformed file. Nothing to do.
	if !s.started {
		return nil
	}
	// Flush the per-track Enforcer reorder buffers (their newest packet is held
	// until now), then any audio still held in the Aligner pending queues, so
	// no tail packets are lost. Enforcers are drained first because a flushed
	// video keyframe may unlock the audio aligners' sync start.
	for trackNum, enf := range s.enforcers {
		var processed []*core.Packet
		emit := func(out *core.Packet) { processed = append(processed, out) }
		enf.Flush(emit)
		for _, pp := range processed {
			if err := s.pushThroughAligner(trackNum, pp); err != nil {
				return err
			}
		}
	}
	for trackNum, al := range s.aligners {
		var flushed []*core.Packet
		emit := func(out *core.Packet) { flushed = append(flushed, out) }
		al.Flush(emit)
		for _, ap := range flushed {
			if err := s.writeBlock(trackNum, ap); err != nil {
				return err
			}
			core.ReleasePacket(ap)
		}
	}
	if s.isTS() {
		// MPEG-TS has no trailer, cues, or duration patch.
		return s.tsMux.Close()
	}
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
	// SeekHead (seekable only): points at Info, Tracks, and Cues so players
	// can locate them without scanning. ARCHITECTURE.md §5.C requires this;
	// it was previously omitted despite the doc comment claiming it.
	if s.ws.seekable {
		infoPos := s.header.InfoStart - int64(s.header.SegmentStart)
		tracksPos := s.header.TracksStart - int64(s.header.SegmentStart)
		if err := webm.WriteSeekHead(s.ws, infoPos, tracksPos, cuesPos); err != nil {
			return err
		}
	}
	// Segment size patch (seekable only).
	if s.ws.seekable {
		end := s.ws.offset()
		segLen := uint64(end - int64(s.header.SegmentStart))
		if err := webm.PatchSegmentSize(s.ws, s.header.SegmentSizeOff, 8, segLen); err != nil {
			return err
		}
	}
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
	errUnknownTrack     = errPtr("unknown track")
	errUnsupportedCodec = errPtr("codec not permitted in output container")
	errNotSeekable      = errPtr("writer is not seekable")
	errTrackAfterStart  = errPtr("AddTrack called after first WritePacket")
)

type errPtr string

func (e errPtr) Error() string { return string(e) }
