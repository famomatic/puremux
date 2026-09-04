package webm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/ebml"
)

const defaultTimestampScale = uint64(1_000_000)

type DemuxTrack struct {
	Number            int
	UID               uint64
	Codec             core.CodecType
	IsVideo           bool
	Width             int
	Height            int
	Channels          int
	SampleRate        float64
	CodecPrivate      []byte
	DefaultDurationNS uint64
	CodecDelayNS      uint64
	SeekPreRollNS     uint64
	Default           bool
	Name              string
	Language          string
}

type DemuxPacket struct {
	TrackNum         int
	TimestampNS      int64
	DurationNS       int64
	DiscardPaddingNS int64
	Keyframe         bool
	Invisible        bool
	Discardable      bool
	Data             []byte
	Position         int64
}

type DemuxMetadata struct {
	DocType          string
	TimestampScaleNS uint64
	Duration         time.Duration
	DurationKnown    bool
	Tags             map[string]string
}

type cueEntry struct {
	timeTicks uint64
	track     uint64
	position  uint64
}

type clusterEntry struct {
	timeTicks uint64
	position  int64
}

type matroskaElement struct {
	ebml.Header
	start   int64
	payload int64
	end     int64
}

// DemuxReader is the seekable, packet-accurate WebM/Matroska reader used by
// pkg/media. It scans element boundaries and indexes clusters without reading
// compressed payloads during Open.
type DemuxReader struct {
	mu sync.Mutex
	rs io.ReadSeeker

	size         int64
	segmentStart int64
	segmentEnd   int64
	firstCluster int64

	metadata DemuxMetadata
	tracks   []DemuxTrack
	trackMap map[int]*DemuxTrack
	cues     []cueEntry
	clusters []clusterEntry

	clusterEnd     int64
	clusterUnknown bool
	clusterTicks   uint64
	inCluster      bool
	pending        []*DemuxPacket
	closed         bool
}

func NewDemuxReader(rs io.ReadSeeker) (*DemuxReader, error) {
	if rs == nil {
		return nil, errors.New("webm: nil reader")
	}
	rd := &DemuxReader{
		rs:           rs,
		firstCluster: -1,
		metadata: DemuxMetadata{
			TimestampScaleNS: defaultTimestampScale,
			Tags:             make(map[string]string),
		},
		trackMap: make(map[int]*DemuxTrack),
	}
	if err := rd.scan(); err != nil {
		return nil, err
	}
	return rd, nil
}

func (rd *DemuxReader) Metadata() DemuxMetadata {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	m := rd.metadata
	m.Tags = cloneStringMap(m.Tags)
	return m
}

func (rd *DemuxReader) Tracks() []DemuxTrack {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	out := make([]DemuxTrack, len(rd.tracks))
	copy(out, rd.tracks)
	for i := range out {
		out[i].CodecPrivate = append([]byte(nil), out[i].CodecPrivate...)
	}
	return out
}

func (rd *DemuxReader) Close() error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.closed = true
	rd.pending = nil
	return nil
}

func (rd *DemuxReader) scan() error {
	cur, err := rd.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	size, err := rd.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size < 0 {
		return errors.New("webm: invalid source size")
	}
	rd.size = size
	if _, err := rd.rs.Seek(0, io.SeekStart); err != nil {
		return err
	}

	e, err := rd.readElement()
	if err != nil || e.ID != idEBML || e.Unknown {
		return errors.New("webm: missing EBML header")
	}
	if err := rd.parseEBML(e); err != nil {
		return err
	}
	e, err = rd.readElement()
	if err != nil || e.ID != idSegment {
		return errors.New("webm: missing Segment")
	}
	rd.segmentStart = e.payload
	rd.segmentEnd = e.end
	if e.Unknown {
		rd.segmentEnd = rd.size
	}
	var durationTicks float64
	var haveDuration bool
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= rd.segmentEnd {
			break
		}
		child, err := rd.readElement()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		switch child.ID {
		case idInfo:
			v, ok, err := rd.parseInfo(child)
			if err != nil {
				return err
			}
			if ok {
				durationTicks, haveDuration = v, true
			}
		case idTracks:
			if err := rd.parseDemuxTracks(child); err != nil {
				return err
			}
		case idCues:
			if err := rd.parseCues(child); err != nil {
				return err
			}
		case idTags:
			if err := rd.parseTags(child); err != nil {
				return err
			}
		case idCluster:
			if rd.firstCluster < 0 {
				rd.firstCluster = child.start
			}
			next, ticks, err := rd.scanCluster(child)
			if err != nil {
				return err
			}
			rd.clusters = append(rd.clusters, clusterEntry{timeTicks: ticks, position: child.start})
			if _, err := rd.rs.Seek(next, io.SeekStart); err != nil {
				return err
			}
		default:
			if err := rd.skipElement(child); err != nil {
				return err
			}
		}
	}
	if len(rd.tracks) == 0 {
		return errors.New("webm: no tracks")
	}
	// Appending tracks may have reallocated the backing slice, so rebuild all
	// pointers only after the complete Tracks element has been parsed.
	rd.trackMap = make(map[int]*DemuxTrack, len(rd.tracks))
	for i := range rd.tracks {
		rd.trackMap[rd.tracks[i].Number] = &rd.tracks[i]
	}
	if haveDuration && durationTicks >= 0 {
		ns := durationTicks * float64(rd.metadata.TimestampScaleNS)
		if ns <= float64(math.MaxInt64) {
			rd.metadata.Duration = time.Duration(ns)
			rd.metadata.DurationKnown = true
		}
	}
	if rd.firstCluster >= 0 {
		if _, err := rd.rs.Seek(rd.firstCluster, io.SeekStart); err != nil {
			return err
		}
	} else if _, err := rd.rs.Seek(cur, io.SeekStart); err != nil {
		return err
	}
	return nil
}

func (rd *DemuxReader) NextPacket(ctx context.Context) (*DemuxPacket, error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.closed {
		return nil, errors.New("webm: reader closed")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(rd.pending) > 0 {
			p := rd.pending[0]
			rd.pending[0] = nil
			rd.pending = rd.pending[1:]
			return p, nil
		}
		if rd.inCluster {
			pos, _ := rd.rs.Seek(0, io.SeekCurrent)
			if !rd.clusterUnknown && pos >= rd.clusterEnd {
				rd.inCluster = false
				continue
			}
			child, err := rd.readElement()
			if err != nil {
				return nil, err
			}
			if rd.clusterUnknown && isSegmentLevelOne(child.ID) {
				if _, err := rd.rs.Seek(child.start, io.SeekStart); err != nil {
					return nil, err
				}
				rd.inCluster = false
				continue
			}
			switch child.ID {
			case idTimestamp:
				v, err := rd.readUint(child)
				if err != nil {
					return nil, err
				}
				rd.clusterTicks = v
			case idSimpleBlock:
				payload, err := rd.readPayload(child)
				if err != nil {
					return nil, err
				}
				if err := rd.queueBlock(payload, child.payload, true, 0, 0, false); err != nil {
					return nil, err
				}
			case idBlockGroup:
				if err := rd.parseBlockGroupPacket(child); err != nil {
					return nil, err
				}
			default:
				if err := rd.skipElement(child); err != nil {
					return nil, err
				}
			}
			continue
		}

		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= rd.segmentEnd {
			return nil, io.EOF
		}
		e, err := rd.readElement()
		if err != nil {
			return nil, err
		}
		if e.ID != idCluster {
			if err := rd.skipElement(e); err != nil {
				return nil, err
			}
			continue
		}
		rd.inCluster = true
		rd.clusterUnknown = e.Unknown
		rd.clusterEnd = e.end
		rd.clusterTicks = 0
	}
}

func (rd *DemuxReader) SeekTicks(ctx context.Context, target uint64, track int) (uint64, error) {
	return rd.SeekTicksWithFlags(ctx, target, track, true, false)
}

// SeekTicksWithFlags selects a cue (sync seek) or cluster (SeekAny) in the
// requested direction.
func (rd *DemuxReader) SeekTicksWithFlags(ctx context.Context, target uint64, track int, backward, any bool) (uint64, error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.closed {
		return 0, errors.New("webm: reader closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var pos int64 = -1
	var actual uint64
	choose := func(candidate int64, ticks uint64) {
		if candidate < rd.segmentStart || candidate >= rd.segmentEnd {
			return
		}
		eligible := ticks <= target
		if !backward {
			eligible = ticks >= target
		}
		if !eligible {
			return
		}
		if pos < 0 || (backward && ticks > actual) || (!backward && ticks < actual) {
			pos, actual = candidate, ticks
		}
	}
	if !any {
		for _, cue := range rd.cues {
			if track >= 0 && int(cue.track) != track {
				continue
			}
			candidate, ok := addInt64Uint64(rd.segmentStart, cue.position)
			if ok {
				choose(candidate, cue.timeTicks)
			}
		}
	}
	if pos < 0 {
		for _, cluster := range rd.clusters {
			choose(cluster.position, cluster.timeTicks)
		}
	}
	// Clamp at an endpoint when the requested direction has no candidate.
	if pos < 0 {
		for _, cluster := range rd.clusters {
			if pos < 0 || (backward && cluster.timeTicks < actual) || (!backward && cluster.timeTicks > actual) {
				pos, actual = cluster.position, cluster.timeTicks
			}
		}
	}
	if pos < 0 && rd.firstCluster >= 0 {
		pos = rd.firstCluster
	}
	if pos < 0 {
		return 0, errors.New("webm: no seek point")
	}
	if _, err := rd.rs.Seek(pos, io.SeekStart); err != nil {
		return 0, err
	}
	rd.pending = nil
	rd.inCluster = false
	rd.clusterUnknown = false
	rd.clusterEnd = 0
	rd.clusterTicks = 0
	return actual, nil
}

func (rd *DemuxReader) queueBlock(payload []byte, payloadPos int64, simple bool, blockDuration uint64, discardPadding int64, referenced bool) error {
	b, err := parseBlockPayload(payload)
	if err != nil {
		return err
	}
	t := rd.trackMap[b.track]
	if t == nil {
		return errors.New("webm: block references unknown track")
	}
	if rd.clusterTicks > math.MaxInt64 {
		return errors.New("webm: timestamp overflow")
	}
	clusterTicks := int64(rd.clusterTicks)
	relativeTicks := int64(b.relTimecode)
	if relativeTicks > 0 && clusterTicks > math.MaxInt64-relativeTicks {
		return errors.New("webm: timestamp overflow")
	}
	baseTicks := clusterTicks + relativeTicks
	baseNS, ok := multiplyTimecode(baseTicks, rd.metadata.TimestampScaleNS)
	if !ok {
		return errors.New("webm: timestamp overflow")
	}
	durations := make([]int64, len(b.frames))
	if blockDuration > 0 {
		if blockDuration > math.MaxInt64/rd.metadata.TimestampScaleNS {
			return errors.New("webm: block duration overflow")
		}
		total := int64(blockDuration * rd.metadata.TimestampScaleNS)
		for i := range durations {
			durations[i] = total / int64(len(durations))
		}
		durations[len(durations)-1] += total % int64(len(durations))
	} else {
		for i, frame := range b.frames {
			switch {
			case t.Codec == core.CodecOpus:
				durations[i] = int64(core.OpusPacketDuration(frame.data))
			case t.DefaultDurationNS > 0 && t.DefaultDurationNS <= math.MaxInt64:
				durations[i] = int64(t.DefaultDurationNS)
			}
		}
	}
	ts := baseNS
	for i, frame := range b.frames {
		key := simple && b.flags&0x80 != 0
		if !t.IsVideo {
			key = true
		} else if !simple {
			key = !referenced
		}
		padding := int64(0)
		if i == len(b.frames)-1 {
			padding = discardPadding
		}
		rd.pending = append(rd.pending, &DemuxPacket{
			TrackNum:         b.track,
			TimestampNS:      ts,
			DurationNS:       durations[i],
			DiscardPaddingNS: padding,
			Keyframe:         key,
			Invisible:        b.flags&0x08 != 0,
			Discardable:      simple && b.flags&0x01 != 0,
			Data:             frame.data,
			Position:         payloadPos + int64(frame.offset),
		})
		if durations[i] > 0 {
			if ts > math.MaxInt64-durations[i] {
				return errors.New("webm: laced timestamp overflow")
			}
			ts += durations[i]
		}
	}
	return nil
}

func (rd *DemuxReader) parseBlockGroupPacket(group matroskaElement) error {
	if group.Unknown || group.end < 0 {
		return errors.New("webm: unknown-size BlockGroup")
	}
	end := group.end
	var payload []byte
	var payloadPos int64
	var blockDuration uint64
	var discardPadding int64
	var referenced bool
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idBlock:
			payloadPos = e.payload
			payload, err = rd.readPayload(e)
		case idBlockDuration:
			blockDuration, err = rd.readUint(e)
		case idDiscardPadding:
			discardPadding, err = rd.readInt(e)
		case idReferenceBlock:
			referenced = true
			err = rd.skipElement(e)
		default:
			err = rd.skipElement(e)
		}
		if err != nil {
			return err
		}
	}
	if payload == nil {
		return nil
	}
	return rd.queueBlock(payload, payloadPos, false, blockDuration, discardPadding, referenced)
}

func (rd *DemuxReader) parseEBML(parent matroskaElement) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		if e.ID == idDocType {
			s, err := rd.readString(e)
			if err != nil {
				return err
			}
			rd.metadata.DocType = strings.ToLower(s)
		} else if err := rd.skipElement(e); err != nil {
			return err
		}
	}
}

func (rd *DemuxReader) parseInfo(parent matroskaElement) (float64, bool, error) {
	var duration float64
	var have bool
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return duration, have, nil
		}
		e, err := rd.readElement()
		if err != nil {
			return 0, false, err
		}
		switch e.ID {
		case idTimestampScale:
			v, err := rd.readUint(e)
			if err != nil || v == 0 {
				return 0, false, errors.New("webm: invalid TimestampScale")
			}
			rd.metadata.TimestampScaleNS = v
		case idDuration:
			duration, err = rd.readFloat(e)
			have = err == nil
			if err != nil {
				return 0, false, err
			}
		case idTitle, idMuxingApp, idWritingApp:
			v, err := rd.readString(e)
			if err != nil {
				return 0, false, err
			}
			key := map[uint32]string{idTitle: "title", idMuxingApp: "muxing_app", idWritingApp: "writing_app"}[e.ID]
			rd.metadata.Tags[key] = v
		default:
			if err := rd.skipElement(e); err != nil {
				return 0, false, err
			}
		}
	}
}

func (rd *DemuxReader) parseDemuxTracks(parent matroskaElement) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		if e.ID != idTrackEntry {
			if err := rd.skipElement(e); err != nil {
				return err
			}
			continue
		}
		t, err := rd.parseDemuxTrack(e)
		if err != nil {
			return err
		}
		if t.Number <= 0 || rd.trackMap[t.Number] != nil {
			return errors.New("webm: invalid or duplicate track number")
		}
		rd.tracks = append(rd.tracks, t)
		rd.trackMap[t.Number] = &rd.tracks[len(rd.tracks)-1]
	}
}

func (rd *DemuxReader) parseDemuxTrack(parent matroskaElement) (DemuxTrack, error) {
	t := DemuxTrack{Default: true, Language: "eng"}
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return t, nil
		}
		e, err := rd.readElement()
		if err != nil {
			return t, err
		}
		switch e.ID {
		case idTrackNumber:
			v, err := rd.readUint(e)
			if err != nil || v > math.MaxInt {
				return t, errors.New("webm: invalid TrackNumber")
			}
			t.Number = int(v)
		case idTrackUID:
			t.UID, err = rd.readUint(e)
		case idTrackType:
			var v uint64
			v, err = rd.readUint(e)
			t.IsVideo = v == trackTypeVideo
		case idFlagDefault:
			var v uint64
			v, err = rd.readUint(e)
			t.Default = v != 0
		case idDefaultDuration:
			t.DefaultDurationNS, err = rd.readUint(e)
		case idCodecDelay:
			t.CodecDelayNS, err = rd.readUint(e)
		case idSeekPreRoll:
			t.SeekPreRollNS, err = rd.readUint(e)
		case idName:
			t.Name, err = rd.readString(e)
		case idLanguage:
			t.Language, err = rd.readString(e)
		case idCodecID:
			var v string
			v, err = rd.readString(e)
			t.Codec = codecTypeFromString(v)
		case idCodecPrivate:
			t.CodecPrivate, err = rd.readPayload(e)
		case idVideo:
			err = rd.parseDemuxVideo(e, &t)
		case idAudio:
			err = rd.parseDemuxAudio(e, &t)
		default:
			err = rd.skipElement(e)
		}
		if err != nil {
			return t, err
		}
	}
}

func (rd *DemuxReader) parseDemuxVideo(parent matroskaElement, t *DemuxTrack) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idPixelWidth:
			v, err := rd.readUint(e)
			if err != nil {
				return err
			}
			t.Width = int(v)
		case idPixelHeight:
			v, err := rd.readUint(e)
			if err != nil {
				return err
			}
			t.Height = int(v)
		default:
			if err := rd.skipElement(e); err != nil {
				return err
			}
		}
	}
}

func (rd *DemuxReader) parseDemuxAudio(parent matroskaElement, t *DemuxTrack) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			if t.Channels == 0 {
				t.Channels = 1
			}
			if t.SampleRate == 0 {
				t.SampleRate = 8000
			}
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idChannels:
			v, err := rd.readUint(e)
			if err != nil {
				return err
			}
			t.Channels = int(v)
		case idSamplingFrequency:
			v, err := rd.readFloat(e)
			if err != nil {
				return err
			}
			t.SampleRate = v
		default:
			if err := rd.skipElement(e); err != nil {
				return err
			}
		}
	}
}

func (rd *DemuxReader) parseCues(parent matroskaElement) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		if e.ID != idCuePoint {
			if err := rd.skipElement(e); err != nil {
				return err
			}
			continue
		}
		if err := rd.parseCuePoint(e); err != nil {
			return err
		}
	}
}

func (rd *DemuxReader) parseCuePoint(parent matroskaElement) error {
	var cueTime uint64
	var positions []cueEntry
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			for i := range positions {
				positions[i].timeTicks = cueTime
				rd.cues = append(rd.cues, positions[i])
			}
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idCueTime:
			cueTime, err = rd.readUint(e)
		case idCueTrackPositions:
			var c cueEntry
			c, err = rd.parseCueTrackPosition(e)
			positions = append(positions, c)
		default:
			err = rd.skipElement(e)
		}
		if err != nil {
			return err
		}
	}
}

func (rd *DemuxReader) parseCueTrackPosition(parent matroskaElement) (cueEntry, error) {
	var c cueEntry
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			return c, nil
		}
		e, err := rd.readElement()
		if err != nil {
			return c, err
		}
		switch e.ID {
		case idCueTrack:
			c.track, err = rd.readUint(e)
		case idCueClusterPosition:
			c.position, err = rd.readUint(e)
		default:
			err = rd.skipElement(e)
		}
		if err != nil {
			return c, err
		}
	}
}

func (rd *DemuxReader) parseTags(parent matroskaElement) error {
	return rd.walkTags(parent.end)
}

func (rd *DemuxReader) walkTags(end int64) error {
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= end {
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idTags, idTag:
			if err := rd.walkTags(e.end); err != nil {
				return err
			}
		case idSimpleTag:
			if err := rd.parseSimpleTag(e); err != nil {
				return err
			}
		default:
			if err := rd.skipElement(e); err != nil {
				return err
			}
		}
	}
}

func (rd *DemuxReader) parseSimpleTag(parent matroskaElement) error {
	var name, value string
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= parent.end {
			if name != "" {
				rd.metadata.Tags[strings.ToLower(name)] = value
			}
			return nil
		}
		e, err := rd.readElement()
		if err != nil {
			return err
		}
		switch e.ID {
		case idTagName:
			name, err = rd.readString(e)
		case idTagString:
			value, err = rd.readString(e)
		case idSimpleTag:
			err = rd.parseSimpleTag(e)
		default:
			err = rd.skipElement(e)
		}
		if err != nil {
			return err
		}
	}
}

func (rd *DemuxReader) scanCluster(cluster matroskaElement) (int64, uint64, error) {
	var ticks uint64
	if !cluster.Unknown {
		for {
			pos, _ := rd.rs.Seek(0, io.SeekCurrent)
			if pos >= cluster.end {
				return cluster.end, ticks, nil
			}
			e, err := rd.readElement()
			if err != nil {
				return 0, 0, err
			}
			if e.ID == idTimestamp {
				ticks, err = rd.readUint(e)
				if err != nil {
					return 0, 0, err
				}
				return cluster.end, ticks, nil
			}
			if err := rd.skipElement(e); err != nil {
				return 0, 0, err
			}
		}
	}
	for {
		pos, _ := rd.rs.Seek(0, io.SeekCurrent)
		if pos >= rd.segmentEnd {
			return pos, ticks, nil
		}
		e, err := rd.readElement()
		if err != nil {
			return 0, 0, err
		}
		if isSegmentLevelOne(e.ID) {
			return e.start, ticks, nil
		}
		if e.ID == idTimestamp {
			ticks, err = rd.readUint(e)
		} else {
			err = rd.skipElement(e)
		}
		if err != nil {
			return 0, 0, err
		}
	}
}

func (rd *DemuxReader) readElement() (matroskaElement, error) {
	start, err := rd.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return matroskaElement{}, err
	}
	h, err := ebml.ReadHeader(rd.rs)
	if err != nil {
		return matroskaElement{}, fmt.Errorf("webm: element at offset %d: %w", start, err)
	}
	payload, err := rd.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return matroskaElement{}, err
	}
	e := matroskaElement{Header: h, start: start, payload: payload, end: -1}
	if !h.Unknown {
		if h.Size > math.MaxInt64 || payload > math.MaxInt64-int64(h.Size) {
			return matroskaElement{}, errors.New("webm: element size overflow")
		}
		e.end = payload + int64(h.Size)
		if e.end > rd.size {
			return matroskaElement{}, io.ErrUnexpectedEOF
		}
	}
	return e, nil
}

func (rd *DemuxReader) skipElement(e matroskaElement) error {
	if e.Unknown || e.end < 0 {
		return errors.New("webm: cannot skip unknown-size element")
	}
	_, err := rd.rs.Seek(e.end, io.SeekStart)
	return err
}

func (rd *DemuxReader) readPayload(e matroskaElement) ([]byte, error) {
	if e.Unknown || e.Size > maxElementSize || e.Size > uint64(math.MaxInt) {
		return nil, errors.New("webm: invalid or excessive element payload")
	}
	b := make([]byte, int(e.Size))
	_, err := io.ReadFull(rd.rs, b)
	return b, err
}

func (rd *DemuxReader) readUint(e matroskaElement) (uint64, error) {
	if e.Unknown || e.Size == 0 || e.Size > 8 {
		return 0, errors.New("webm: invalid unsigned integer")
	}
	b, err := rd.readPayload(e)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, nil
}

func (rd *DemuxReader) readInt(e matroskaElement) (int64, error) {
	u, err := rd.readUint(e)
	if err != nil {
		return 0, err
	}
	shift := uint(64 - 8*e.Size)
	return int64(u<<shift) >> shift, nil
}

func (rd *DemuxReader) readFloat(e matroskaElement) (float64, error) {
	b, err := rd.readPayload(e)
	if err != nil {
		return 0, err
	}
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	default:
		return 0, errors.New("webm: float must be 4 or 8 bytes")
	}
}

func (rd *DemuxReader) readString(e matroskaElement) (string, error) {
	b, err := rd.readPayload(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func isSegmentLevelOne(id uint32) bool {
	switch id {
	case idSeekHead, idInfo, idCluster, idTracks, idCues, idTags, 0x1941A469, 0x1043A770:
		return true
	default:
		return false
	}
}

func multiplyTimecode(ticks int64, scale uint64) (int64, bool) {
	if scale > math.MaxInt64 {
		return 0, false
	}
	s := int64(scale)
	if ticks > 0 && ticks > math.MaxInt64/s {
		return 0, false
	}
	if ticks < 0 && ticks < math.MinInt64/s {
		return 0, false
	}
	return ticks * s, true
}

func addInt64Uint64(base int64, delta uint64) (int64, bool) {
	if delta > math.MaxInt64 || base > math.MaxInt64-int64(delta) {
		return 0, false
	}
	return base + int64(delta), true
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
