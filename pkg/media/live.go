package media

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/preprocessor"
)

// ErrLiveBufferFull reports that one live stream stopped advancing while
// another kept producing packets, exhausting the bounded interleave queue.
var ErrLiveBufferFull = errors.New("media: live interleave buffer full")

// LiveIngestOptions configure the explicitly selected live packet
// normalization path. Zero values select the defaults returned by
// DefaultLiveIngestOptions. The ordinary Muxer remains preprocessor-free.
type LiveIngestOptions struct {
	MaxBufferPackets     int
	JitterWindow         time.Duration
	GapThreshold         time.Duration
	MinMonotonicStep     time.Duration
	MaxInterleavePackets int
}

// DefaultLiveIngestOptions returns bounded settings matching the historical
// live path: a 400 ms jitter window and 64 packets per stream. A caller whose
// output clock is coarser than nanoseconds should set MinMonotonicStep to at
// least that clock granularity (1 ms is a conservative live-output choice).
// AddStream always raises the configured value to one stream TimeBase tick,
// so a repaired DTS cannot collapse at the public Muxer boundary.
func DefaultLiveIngestOptions() LiveIngestOptions {
	return LiveIngestOptions{
		MaxBufferPackets:     64,
		JitterWindow:         400 * time.Millisecond,
		GapThreshold:         100 * time.Millisecond,
		MinMonotonicStep:     time.Nanosecond,
		MaxInterleavePackets: 256,
	}
}

// LiveStreamMetrics expose packet-level degradation introduced by the
// explicitly selected live normalization path.
type LiveStreamMetrics struct {
	DroppedOverflow     uint64
	DroppedOutOfOrder   uint64
	DetectedGaps        uint64
	AudioPacketsDropped uint64
}

// LiveMuxer is an opt-in normalization layer for compressed live packets. It
// implements Muxer while keeping the wrapped muxer's exact serializer contract
// unchanged. Generic WritePacket calls receive bounded jitter repair, strict
// DTS monotonicity, keyframe-first video, A/V start alignment, duration
// completion, and cross-stream ordering.
//
// WriteVideo additionally adapts decode-clock-only Annex-B H.264/HEVC by
// deriving presentation order from POC. WriteADTS splits AAC ADTS chunks and
// derives exact per-frame timing. Every input payload is copied before return
// because normalization may retain packets. LiveMuxer is not safe for
// concurrent use.
type LiveMuxer struct {
	muxer         Muxer
	cfg           preprocessor.Config
	maxInterleave int
	detectors     *core.DetectorRegistry
	streams       []*liveStreamState
	streamByIndex map[int]*liveStreamState
	queue         []liveQueuedPacket
	syncVideo     *liveStreamState
	started       bool
	closed        bool
	draining      bool
	processErr    error
	closeErr      error
}

type liveStreamState struct {
	stream       Stream
	outputIndex  int
	codec        core.CodecType
	video        bool
	audio        bool
	enforcer     *preprocessor.Enforcer
	aligner      *preprocessor.Aligner
	parser       core.PictureOrderParser
	presentation *preprocessor.PresentationSynthesizer
	pending      *core.Packet
	lastDuration time.Duration
	inputMode    liveInputMode
}

type liveInputMode uint8

const (
	liveInputUnset liveInputMode = iota
	liveInputPacket
	liveInputVideo
	liveInputADTS
)

type liveQueuedPacket struct {
	state    *liveStreamState
	packet   *core.Packet
	duration time.Duration
}

// NewLiveMuxer wraps a muxer with opt-in compressed-packet normalization.
// Close flushes normalization state and then closes the wrapped muxer; the
// destination writer remains caller-owned.
func NewLiveMuxer(muxer Muxer, opts LiveIngestOptions) (*LiveMuxer, error) {
	if muxer == nil {
		return nil, ErrInvalidData
	}
	cfg, maxInterleave, err := normalizeLiveOptions(opts)
	if err != nil {
		return nil, err
	}
	return &LiveMuxer{
		muxer:         muxer,
		cfg:           cfg,
		maxInterleave: maxInterleave,
		detectors:     core.NewDetectorRegistry(),
		streamByIndex: make(map[int]*liveStreamState),
	}, nil
}

func normalizeLiveOptions(opts LiveIngestOptions) (preprocessor.Config, int, error) {
	if opts.MaxBufferPackets < 0 || opts.JitterWindow < 0 || opts.GapThreshold < 0 ||
		opts.MinMonotonicStep < 0 || opts.MaxInterleavePackets < 0 {
		return preprocessor.Config{}, 0, ErrInvalidData
	}
	defaults := DefaultLiveIngestOptions()
	if opts.MaxBufferPackets == 0 {
		opts.MaxBufferPackets = defaults.MaxBufferPackets
	}
	if opts.JitterWindow == 0 {
		opts.JitterWindow = defaults.JitterWindow
	}
	if opts.GapThreshold == 0 {
		opts.GapThreshold = defaults.GapThreshold
	}
	if opts.MinMonotonicStep == 0 {
		opts.MinMonotonicStep = defaults.MinMonotonicStep
	}
	if opts.MaxInterleavePackets == 0 {
		opts.MaxInterleavePackets = defaults.MaxInterleavePackets
	}
	return preprocessor.Config{
		MaxBufferSize:             opts.MaxBufferPackets,
		MaxBufferDuration:         uint64(opts.JitterWindow),
		InterpolationGapThreshold: uint64(opts.GapThreshold),
		MinMonotonicStep:          uint64(opts.MinMonotonicStep),
	}, opts.MaxInterleavePackets, nil
}

// AddStream registers a stream with the wrapped muxer. The wrapped muxer owns
// codec/container compatibility checks. All streams must be registered before
// the first valid write.
func (l *LiveMuxer) AddStream(stream Stream) (int, error) {
	if l.closed {
		return 0, ErrClosed
	}
	if l.started {
		return 0, ErrInvalidData
	}
	if !stream.TimeBase.Valid() || stream.TimeBase.Num <= 0 {
		return 0, ErrInvalidData
	}
	streamTick, ok := liveTimeBaseStep(stream.TimeBase)
	if !ok || streamTick <= 0 {
		return 0, ErrInvalidData
	}
	codec := coreCodec(stream.Codec)
	video := stream.Type == MediaVideo
	audio := stream.Type == MediaAudio
	outputIndex, err := l.muxer.AddStream(stream)
	if err != nil {
		return 0, err
	}
	if _, exists := l.streamByIndex[outputIndex]; exists {
		return 0, ErrInvalidData
	}
	detector := l.detectors.Detector(codec)
	streamCfg := l.cfg
	if streamTick > time.Duration(streamCfg.MinMonotonicStep) {
		streamCfg.MinMonotonicStep = uint64(streamTick)
	}
	state := &liveStreamState{
		stream:      stream,
		outputIndex: outputIndex,
		codec:       codec,
		video:       video,
		audio:       audio,
		enforcer:    preprocessor.NewEnforcer(streamCfg),
	}
	if video || audio {
		state.aligner = preprocessor.NewAlignerForSessionWithLimit(detector, video, false, l.cfg.MaxBufferSize)
	}
	if video && (codec == core.CodecH264 || codec == core.CodecHEVC) {
		state.parser = core.NewPictureOrderParser(codec)
		state.presentation = preprocessor.NewPresentationSynthesizer(l.cfg)
	}
	if video && l.syncVideo == nil {
		l.syncVideo = state
	}
	l.streams = append(l.streams, state)
	l.streamByIndex[outputIndex] = state
	return outputIndex, nil
}

// WritePacket normalizes one generic compressed packet before forwarding it
// to the wrapped muxer. PTS and DTS are required; Duration may be omitted and
// is then derived with bounded one-packet lookahead. The caller retains packet
// ownership and may reuse Data after this method returns.
func (l *LiveMuxer) WritePacket(ctx context.Context, packet *Packet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closed {
		return ErrClosed
	}
	if l.processErr != nil {
		return l.processErr
	}
	if packet == nil {
		return nil
	}
	state, ok := l.streamByIndex[packet.StreamIndex]
	if !ok || !packet.PTS.Valid || !packet.DTS.Valid {
		return ErrInvalidData
	}
	pts, okPTS := packet.PTS.Duration(state.stream.TimeBase)
	dts, okDTS := packet.DTS.Duration(state.stream.TimeBase)
	if !okPTS || !okDTS {
		return ErrInvalidData
	}
	duration := time.Duration(0)
	if packet.Duration.Valid {
		var ok bool
		duration, ok = packet.Duration.Duration(state.stream.TimeBase)
		if !ok || duration <= 0 {
			return ErrInvalidData
		}
	}
	if !state.claimInputMode(liveInputPacket) {
		return ErrInvalidData
	}
	l.start()
	p := core.AcquirePacket()
	p.Data = append(p.Data[:0], packet.Data...)
	p.PTS, p.DTS, p.Duration = pts, dts, duration
	p.Codec = state.codec
	p.TrackID = state.outputIndex
	p.Flags = uint16(packet.Flags)
	p.Pos = packet.Pos
	p.DiscardPadding = packet.DiscardPadding
	p.IsKeyframe = packet.Keyframe()
	if state.video && !p.IsKeyframe {
		p.IsKeyframe = l.detectors.Detector(state.codec).IsKeyframe(packet.Data)
	}
	l.processEnforcer(ctx, state, p)
	return l.processErr
}

// WriteVideo writes one H.264/HEVC Annex-B access unit whose timestamp is a
// decode-order clock value in the registered stream's TimeBase. Presentation
// time and keyframe status are derived from bounded codec-header inspection.
func (l *LiveMuxer) WriteVideo(ctx context.Context, streamIndex int, au []byte, decodeTime int64) error {
	state, err := l.streamForWrite(ctx, streamIndex)
	if err != nil {
		return err
	}
	if !state.video || state.presentation == nil || len(au) == 0 {
		return ErrInvalidData
	}
	clock, ok := state.stream.TimeBase.Duration(decodeTime)
	if !ok {
		return ErrInvalidData
	}
	if !state.claimInputMode(liveInputVideo) {
		return ErrInvalidData
	}
	l.start()
	p := core.AcquirePacket()
	p.Data = append(p.Data[:0], au...)
	p.PTS, p.DTS = clock, clock
	p.Codec = state.codec
	p.TrackID = state.outputIndex
	p.Pos = -1
	p.IsKeyframe = l.detectors.Detector(state.codec).IsKeyframe(au)
	poc, picture := state.parser.ParseAU(au)
	state.presentation.Process(p, poc, picture, func(out *core.Packet) {
		l.processEnforcer(ctx, state, out)
	})
	return l.processErr
}

// WriteADTS splits a chunk into complete ADTS frames. Leading noise is skipped,
// each complete frame is copied and timestamped from its spec-defined sample
// count, and a truncated tail is ignored. A chunk containing no complete valid
// frame returns ErrInvalidData.
func (l *LiveMuxer) WriteADTS(ctx context.Context, streamIndex int, chunk []byte, timestamp int64) error {
	state, err := l.streamForWrite(ctx, streamIndex)
	if err != nil {
		return err
	}
	if !state.audio || state.codec != core.CodecAAC {
		return ErrInvalidData
	}
	clock, ok := state.stream.TimeBase.Duration(timestamp)
	if !ok {
		return ErrInvalidData
	}
	frameCount := 0
	frameRate := 0
	frameChannels := 0
	var samplesBefore int64
	var scanErr error
	core.ForEachADTSFrame(chunk, func(_ []byte, info core.ADTSFrameInfo) bool {
		if err := ctx.Err(); err != nil {
			scanErr = err
			return false
		}
		duration := info.Duration()
		if duration <= 0 ||
			(state.stream.SampleRate > 0 && info.SampleRate != state.stream.SampleRate) ||
			(state.stream.Channels > 0 && info.Channels > 0 && info.Channels != state.stream.Channels) {
			scanErr = ErrInvalidData
			return false
		}
		if frameCount == 0 {
			frameRate = info.SampleRate
			frameChannels = info.Channels
		} else if info.SampleRate != frameRate || info.Channels != frameChannels {
			scanErr = ErrInvalidData
			return false
		}
		frameTimeBase := Rational{Num: 1, Den: int64(frameRate)}
		offset, ok := frameTimeBase.Duration(samplesBefore)
		if !ok || (offset > 0 && clock > time.Duration(math.MaxInt64)-offset) ||
			int64(info.Samples) > math.MaxInt64-samplesBefore {
			scanErr = ErrInvalidData
			return false
		}
		samplesBefore += int64(info.Samples)
		frameCount++
		return true
	})
	if scanErr != nil {
		return scanErr
	}
	if frameCount == 0 {
		return ErrInvalidData
	}
	if !state.claimInputMode(liveInputADTS) {
		return ErrInvalidData
	}
	l.start()
	samplesBefore = 0
	frameTimeBase := Rational{Num: 1, Den: int64(frameRate)}
	var writeErr error
	core.ForEachADTSFrame(chunk, func(frame []byte, info core.ADTSFrameInfo) bool {
		if err := ctx.Err(); err != nil {
			writeErr = err
			return false
		}
		offset, ok := frameTimeBase.Duration(samplesBefore)
		if !ok {
			writeErr = ErrInvalidData
			return false
		}
		p := core.AcquirePacket()
		p.Data = append(p.Data[:0], frame...)
		p.PTS, p.DTS = clock+offset, clock+offset
		p.Duration = info.Duration()
		p.Codec = state.codec
		p.TrackID = state.outputIndex
		p.Pos = -1
		l.processEnforcer(ctx, state, p)
		if l.processErr != nil {
			return false
		}
		samplesBefore += int64(info.Samples)
		return true
	})
	if writeErr != nil {
		return writeErr
	}
	return l.processErr
}

func (s *liveStreamState) claimInputMode(mode liveInputMode) bool {
	if s.inputMode == liveInputUnset {
		s.inputMode = mode
	}
	return s.inputMode == mode
}

func (l *LiveMuxer) streamForWrite(ctx context.Context, streamIndex int) (*liveStreamState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.closed {
		return nil, ErrClosed
	}
	if l.processErr != nil {
		return nil, l.processErr
	}
	state, ok := l.streamByIndex[streamIndex]
	if !ok {
		return nil, ErrInvalidData
	}
	return state, nil
}

func (l *LiveMuxer) start() {
	if l.started {
		return
	}
	l.started = true
	for _, state := range l.streams {
		if state.audio {
			state.aligner.SetExpectsVideoSync(l.syncVideo != nil, core.ReleasePacket)
		}
	}
}

func (l *LiveMuxer) processEnforcer(ctx context.Context, state *liveStreamState, p *core.Packet) {
	if l.processErr != nil {
		core.ReleasePacket(p)
		return
	}
	if err := state.enforcer.Process(p, func(out *core.Packet) {
		l.processAligner(ctx, state, out)
	}); err != nil {
		l.processErr = err
	}
}

func (l *LiveMuxer) processAligner(ctx context.Context, state *liveStreamState, p *core.Packet) {
	if l.processErr != nil {
		core.ReleasePacket(p)
		return
	}
	if state == l.syncVideo && p.IsKeyframe {
		for _, other := range l.streams {
			if other.audio {
				other.aligner.SetVideoSyncStart(p.DTS)
			}
		}
	}
	if state.aligner == nil {
		l.completePacket(ctx, state, p)
		return
	}
	if err := state.aligner.Process(p, func(out *core.Packet) {
		l.completePacket(ctx, state, out)
	}); err != nil {
		l.processErr = err
	}
}

func (l *LiveMuxer) completePacket(ctx context.Context, state *liveStreamState, p *core.Packet) {
	if l.processErr != nil {
		core.ReleasePacket(p)
		return
	}
	if state.pending != nil {
		duration := state.pending.Duration
		if duration <= 0 && PacketFlags(p.Flags)&PacketDiscontinuity == 0 {
			var ok bool
			duration, ok = livePacketDuration(state.pending.DTS, p.DTS)
			if !ok {
				core.ReleasePacket(state.pending)
				state.pending = nil
				core.ReleasePacket(p)
				l.processErr = ErrInvalidData
				return
			}
			gapThreshold := time.Duration(l.cfg.InterpolationGapThreshold)
			if gapThreshold > 0 && duration > gapThreshold {
				duration = 0
			}
		}
		if duration <= 0 {
			duration = state.lastDuration
		}
		if duration <= 0 {
			duration = state.stream.DefaultPacket
		}
		if duration > 0 {
			state.lastDuration = duration
		}
		l.enqueue(state, state.pending, duration)
		state.pending = nil
	}
	state.pending = p
	if l.processErr == nil {
		l.flushReady(ctx)
	}
}

func livePacketDuration(start, end time.Duration) (time.Duration, bool) {
	if end <= start {
		return 0, true
	}
	if start < 0 && end > time.Duration(math.MaxInt64)+start {
		return 0, false
	}
	return end - start, true
}

func (l *LiveMuxer) enqueue(state *liveStreamState, p *core.Packet, duration time.Duration) {
	queued := liveQueuedPacket{state: state, packet: p, duration: duration}
	idx := sort.Search(len(l.queue), func(i int) bool {
		return l.queue[i].packet.DTS > p.DTS
	})
	l.queue = append(l.queue, liveQueuedPacket{})
	copy(l.queue[idx+1:], l.queue[idx:])
	l.queue[idx] = queued
	if !l.draining && len(l.queue) > l.maxInterleave {
		l.processErr = ErrLiveBufferFull
	}
}

func (l *LiveMuxer) flushReady(ctx context.Context) {
	if len(l.queue) == 0 || len(l.streams) == 0 {
		return
	}
	var frontier time.Duration
	for i, state := range l.streams {
		if state.pending == nil {
			return
		}
		if i == 0 || state.pending.DTS < frontier {
			frontier = state.pending.DTS
		}
	}
	for len(l.queue) > 0 && l.queue[0].packet.DTS < frontier && l.processErr == nil {
		queued := l.popQueue()
		l.writeQueued(ctx, queued)
	}
}

func (l *LiveMuxer) popQueue() liveQueuedPacket {
	queued := l.queue[0]
	copy(l.queue, l.queue[1:])
	l.queue[len(l.queue)-1] = liveQueuedPacket{}
	l.queue = l.queue[:len(l.queue)-1]
	return queued
}

func (l *LiveMuxer) writeQueued(ctx context.Context, queued liveQueuedPacket) {
	p := queued.packet
	defer core.ReleasePacket(p)
	if err := ctx.Err(); err != nil {
		l.processErr = err
		return
	}
	pts, okPTS := durationToLiveTicks(p.PTS, queued.state.stream.TimeBase)
	dts, okDTS := durationToLiveTicks(p.DTS, queued.state.stream.TimeBase)
	duration, okDuration := durationToLiveTicks(queued.duration, queued.state.stream.TimeBase)
	if !okPTS || !okDTS {
		l.processErr = ErrInvalidData
		return
	}
	if !okDuration || duration <= 0 {
		duration = 1
	}
	flags := PacketFlags(p.Flags)
	if p.IsKeyframe {
		flags |= PacketKeyframe
	}
	if err := l.muxer.WritePacket(ctx, &Packet{
		StreamIndex:    queued.state.outputIndex,
		Data:           p.Data,
		PTS:            KnownTimestamp(pts),
		DTS:            KnownTimestamp(dts),
		Duration:       KnownTimestamp(duration),
		Flags:          flags,
		Pos:            p.Pos,
		DiscardPadding: p.DiscardPadding,
	}); err != nil {
		l.processErr = err
	}
}

var liveNanosecondTimeBase = Rational{Num: 1, Den: int64(time.Second)}

func liveTimeBaseStep(timeBase Rational) (time.Duration, bool) {
	step, ok := timeBase.Duration(1)
	if !ok || step <= 0 {
		return 0, false
	}
	// Duration truncates toward zero. Use the ceiling for a monotonic repair
	// step so repeated nudges can never collapse back onto the same stream tick.
	if ticks, exact := liveNanosecondTimeBase.Rescale(int64(step), timeBase); !exact || ticks != 1 {
		if step == time.Duration(math.MaxInt64) {
			return 0, false
		}
		step++
	}
	return step, true
}

func durationToLiveTicks(value time.Duration, timeBase Rational) (int64, bool) {
	// Start with truncation, then compare the adjacent tick in the direction of
	// value. This restores the nearest destination tick without overflowing an
	// adjusted duration. It also handles clocks between one and two nanoseconds,
	// where an integer half-tick does not exist.
	ticks, ok := liveNanosecondTimeBase.Rescale(int64(value), timeBase)
	if !ok || value == 0 {
		return ticks, ok
	}
	adjacent := ticks
	if value > 0 {
		if ticks == math.MaxInt64 {
			return ticks, true
		}
		adjacent++
	} else {
		if ticks == math.MinInt64 {
			return ticks, true
		}
		adjacent--
	}
	baseDuration, baseOK := timeBase.Duration(ticks)
	adjacentDuration, adjacentOK := timeBase.Duration(adjacent)
	if adjacentOK && (!baseOK || liveDurationDistance(value, adjacentDuration) < liveDurationDistance(value, baseDuration)) {
		return adjacent, true
	}
	return ticks, baseOK
}

func liveDurationDistance(a, b time.Duration) uint64 {
	if (a < 0) == (b < 0) {
		am, bm := liveDurationMagnitude(a), liveDurationMagnitude(b)
		if am >= bm {
			return am - bm
		}
		return bm - am
	}
	return liveDurationMagnitude(a) + liveDurationMagnitude(b)
}

func liveDurationMagnitude(value time.Duration) uint64 {
	if value >= 0 {
		return uint64(value)
	}
	return uint64(-(value + 1)) + 1
}

// Metrics returns the current normalization metrics for a stream index.
func (l *LiveMuxer) Metrics(streamIndex int) (LiveStreamMetrics, bool) {
	state, ok := l.streamByIndex[streamIndex]
	if !ok {
		return LiveStreamMetrics{}, false
	}
	enforcer := state.enforcer.Metrics()
	aligner := preprocessor.Metrics{}
	if state.aligner != nil {
		aligner = state.aligner.Metrics()
	}
	return LiveStreamMetrics{
		DroppedOverflow:     enforcer.DroppedOverflow,
		DroppedOutOfOrder:   enforcer.DroppedOutOfOrder + aligner.DroppedOutOfOrder,
		DetectedGaps:        enforcer.DetectedGaps,
		AudioPacketsDropped: aligner.AudioPacketsDropped,
	}, true
}

// Close drains presentation, jitter, alignment, duration, and interleave state
// in that order, then closes the wrapped muxer. It is idempotent.
func (l *LiveMuxer) Close() error {
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	l.draining = true
	ctx := context.Background()
	if l.processErr == nil {
		for _, state := range l.streams {
			if state.presentation != nil {
				state.presentation.Flush(func(out *core.Packet) {
					l.processEnforcer(ctx, state, out)
				})
			}
		}
	}
	if l.processErr == nil {
		for _, state := range l.streams {
			state.enforcer.Flush(func(out *core.Packet) {
				l.processAligner(ctx, state, out)
			})
		}
	}
	if l.processErr == nil {
		for _, state := range l.streams {
			if state.aligner != nil {
				state.aligner.Flush(func(out *core.Packet) {
					l.completePacket(ctx, state, out)
				})
			}
		}
	}
	if l.processErr == nil {
		for _, state := range l.streams {
			if state.pending == nil {
				continue
			}
			duration := state.pending.Duration
			if duration <= 0 {
				duration = state.lastDuration
			}
			if duration <= 0 {
				duration = state.stream.DefaultPacket
			}
			l.enqueue(state, state.pending, duration)
			state.pending = nil
		}
		for len(l.queue) > 0 && l.processErr == nil {
			l.writeQueued(ctx, l.popQueue())
		}
	}
	if l.processErr != nil {
		l.discardBuffered()
	}
	muxErr := l.muxer.Close()
	if l.processErr != nil {
		l.closeErr = l.processErr
	} else {
		l.closeErr = muxErr
	}
	return l.closeErr
}

func (l *LiveMuxer) discardBuffered() {
	for _, state := range l.streams {
		if state.presentation != nil {
			state.presentation.Flush(core.ReleasePacket)
		}
		state.enforcer.Flush(core.ReleasePacket)
		if state.aligner != nil {
			state.aligner.Flush(core.ReleasePacket)
		}
		core.ReleasePacket(state.pending)
		state.pending = nil
	}
	for len(l.queue) > 0 {
		queued := l.popQueue()
		core.ReleasePacket(queued.packet)
	}
}

var _ Muxer = (*LiveMuxer)(nil)
