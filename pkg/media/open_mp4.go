package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/format/mp4"
)

// OpenMP4WithInit opens an fMP4 media segment using codec and track defaults
// from a separate initialization segment. The returned demuxer owns src.
func OpenMP4WithInit(ctx context.Context, init []byte, src Source) (Demuxer, error) {
	if src == nil || len(init) == 0 {
		return nil, fmt.Errorf("%w: missing fMP4 init or media source", ErrInvalidData)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rs, ok := src.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		return nil, ErrNotSeekable
	}
	var contextual *contextReadSeeker
	readSeeker := io.ReadSeeker(rs)
	if contextSource, ok := src.(ContextSource); ok {
		contextual = &contextReadSeeker{ReadSeeker: readSeeker, source: contextSource, ctx: ctx}
		readSeeker = contextual
	}
	rd, err := mp4.NewFragmentReader(bytes.NewReader(init), readSeeker)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidData, err)
	}
	return newMP4Demuxer(src, rd, contextual), nil
}

type mp4Demuxer struct {
	stateMu    sync.Mutex
	opMu       sync.Mutex
	src        Source
	rd         *mp4.Reader
	streams    []Stream
	trackIndex map[int]int
	info       Info
	contextual *contextReadSeeker
	closed     bool
}

func newMP4Demuxer(src Source, rd *mp4.Reader, contextual *contextReadSeeker) *mp4Demuxer {
	tracks := rd.Tracks()
	streams := make([]Stream, 0, len(tracks))
	trackIndex := make(map[int]int, len(tracks))
	for _, track := range tracks {
		index := len(streams)
		trackIndex[track.Number] = index
		stream := Stream{
			Index:      index,
			ID:         int64(track.ID),
			Type:       MediaAudio,
			Codec:      codecID(track.Codec),
			TimeBase:   Rational{Num: 1, Den: int64(track.Timescale)},
			Language:   track.Language,
			SampleRate: int(track.SampleRate),
			Channels:   track.Channels,
			Width:      track.Width,
			Height:     track.Height,
			Config: CodecConfig{
				Format: mp4ConfigFormat(track.CodecConfigType),
				Data:   append([]byte(nil), track.CodecConfig...),
			},
		}
		if track.IsVideo {
			stream.Type = MediaVideo
		}
		if track.Duration > 0 {
			stream.Duration = KnownTimestamp(int64(track.Duration))
		}
		streams = append(streams, stream)
	}
	duration, scale := rd.MovieDuration()
	info := Info{Format: FormatMP4, FormatName: "mp4", Metadata: rd.Metadata()}
	if duration > 0 && scale > 0 {
		info.Duration, info.DurationKnown = time.Duration(scaleTicksToNS(int64(duration), scale)), true
	}
	return &mp4Demuxer{src: src, rd: rd, streams: streams, trackIndex: trackIndex, info: info, contextual: contextual}
}

func (d *mp4Demuxer) Streams() []Stream { return cloneStreams(d.streams) }

func (d *mp4Demuxer) Info() Info {
	info := d.info
	info.Metadata = cloneMetadata(info.Metadata)
	return info
}

func (d *mp4Demuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := d.ready(ctx); err != nil {
		return nil, err
	}
	sample, err := d.rd.NextSample()
	if err != nil {
		return nil, err
	}
	index, ok := d.trackIndex[sample.TrackNum]
	if !ok {
		return nil, fmt.Errorf("%w: MP4 track %d", ErrInvalidData, sample.TrackNum)
	}
	flags := PacketFlags(0)
	if sample.Keyframe {
		flags |= PacketKeyframe
	}
	return &Packet{
		StreamIndex: index,
		Data:        sample.Data,
		PTS:         KnownTimestamp(sample.PTS),
		DTS:         KnownTimestamp(sample.DTS),
		Duration:    KnownTimestamp(sample.Duration),
		Flags:       flags,
		Pos:         sample.Position,
	}, nil
}

func (d *mp4Demuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := d.ready(ctx); err != nil {
		return SeekResult{}, err
	}
	if err := validateSeekRequest(req, len(d.streams)); err != nil {
		return SeekResult{}, err
	}
	index := req.StreamIndex
	if index < 0 {
		index = 0
		for i := range d.streams {
			if d.streams[i].Type == MediaAudio {
				index = i
				break
			}
		}
	}
	targetNS := req.Target
	if req.StreamIndex >= 0 {
		var ok bool
		targetNS, ok = d.streams[index].TimeBase.Rescale(req.Target, nanosecondTimeBase)
		if !ok {
			return SeekResult{}, errors.New("media: MP4 seek timestamp overflow")
		}
	}
	trackNumber := 0
	for number, mapped := range d.trackIndex {
		if mapped == index {
			trackNumber = number
			break
		}
	}
	actualNS, err := d.rd.SeekNSWithFlags(trackNumber, targetNS, req.Flags&SeekBackward != 0, req.Flags&SeekAny != 0)
	if err != nil {
		return SeekResult{}, err
	}
	result := SeekResult{StreamIndex: req.StreamIndex, Timestamp: actualNS}
	if req.StreamIndex >= 0 {
		var ok bool
		result.Timestamp, ok = nanosecondTimeBase.Rescale(actualNS, d.streams[index].TimeBase)
		if !ok {
			return SeekResult{}, errors.New("media: MP4 seek result overflow")
		}
	}
	return result, nil
}

func (d *mp4Demuxer) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return ErrClosed
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	return nil
}

func (d *mp4Demuxer) Close() error {
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.stateMu.Unlock()
	return d.src.Close()
}

func mp4ConfigFormat(typ string) CodecConfigFormat {
	switch typ {
	case "avcC":
		return CodecConfigAVCC
	case "hvcC":
		return CodecConfigHVCC
	case "asc":
		return CodecConfigASC
	case "dfLa":
		return CodecConfigFLACStreamInfo
	case "dOps":
		return CodecConfigDOPS
	case "av1C":
		return CodecConfigAV1C
	case "vpcC":
		return CodecConfigVPCC
	default:
		return CodecConfigUnknown
	}
}

func scaleTicksToNS(value int64, scale uint32) int64 {
	result, _ := (Rational{Num: 1, Den: int64(scale)}).Rescale(value, nanosecondTimeBase)
	return result
}

var _ Demuxer = (*mp4Demuxer)(nil)
