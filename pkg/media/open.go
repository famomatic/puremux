package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/mp4"
	"github.com/famomatic/puremux/internal/format/ogg"
	"github.com/famomatic/puremux/internal/format/webm"
	mp3bit "github.com/famomatic/puremux/pkg/bitstream/mp3"
)

var nanosecondTimeBase = Rational{Num: 1, Den: 1_000_000_000}

// Open probes src and opens a compressed-packet demuxer. The returned
// demuxer owns src after a successful call. The caller retains ownership when
// Open returns an error.
func Open(ctx context.Context, src Source, opts OpenOptions) (Demuxer, error) {
	if src == nil {
		return nil, errors.New("media: nil source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.MaxProbeBytes < 0 {
		return nil, errors.New("media: MaxProbeBytes must not be negative")
	}
	rs, seekable := src.(io.ReadSeeker)
	if !seekable && opts.FormatHint == FormatUnknown {
		return nil, fmt.Errorf("%w: a non-seekable source requires FormatHint", ErrNotSeekable)
	}
	if !seekable && opts.FormatHint != FormatMPEGTS {
		return nil, fmt.Errorf("%w: %s requires random access", ErrNotSeekable, opts.FormatHint)
	}
	var contextual *contextReadSeeker
	if seekable {
		if contextSource, ok := src.(ContextSource); ok {
			contextual = &contextReadSeeker{ReadSeeker: rs, source: contextSource, ctx: ctx}
			rs = contextual
		}
	}
	format := opts.FormatHint
	if format == FormatUnknown {
		probeBytes := int64(12)
		if opts.MaxProbeBytes > 0 && opts.MaxProbeBytes < probeBytes {
			probeBytes = opts.MaxProbeBytes
		}
		if probeBytes < 4 {
			return nil, errors.New("media: MaxProbeBytes must be at least 4 when probing")
		}
		var head [12]byte
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		n, err := io.ReadFull(rs, head[:probeBytes])
		if _, seekErr := rs.Seek(pos, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}
		if err != nil && n < 4 {
			return nil, fmt.Errorf("%w: input header: %v", ErrInvalidData, err)
		}
		if [4]byte(head[:4]) == [4]byte{0x1a, 0x45, 0xdf, 0xa3} {
			format = FormatMatroska
		} else if [4]byte(head[:4]) == [4]byte{'O', 'g', 'g', 'S'} {
			format = FormatOgg
		} else if n >= 8 && (string(head[4:8]) == "ftyp" || string(head[4:8]) == "styp") {
			format = FormatMP4
		} else if [4]byte(head[:4]) == [4]byte{'f', 'L', 'a', 'C'} {
			format = FormatFLAC
		} else if string(head[:3]) == "ID3" {
			format = FormatMP3
		} else if head[0] == 0xff && head[1]&0xf6 == 0xf0 {
			format = FormatADTS
		} else if _, mp3Err := mp3bit.ParseHeader(head[:4]); mp3Err == nil {
			format = FormatMP3
		} else if head[0] == 0x47 {
			format = FormatMPEGTS
		}
	}
	switch format {
	case FormatWebM, FormatMatroska:
		rd, err := webm.NewDemuxReader(rs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidData, err)
		}
		return newWebMDemuxer(src, rd, contextual), nil
	case FormatOgg:
		rd, err := ogg.NewReader(rs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidData, err)
		}
		return newOggDemuxer(src, rd, contextual), nil
	case FormatMP4:
		rd, err := mp4.NewReader(rs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidData, err)
		}
		return newMP4Demuxer(src, rd, contextual), nil
	case FormatADTS:
		return openADTS(src, rs, contextual)
	case FormatMP3:
		return openMP3(src, rs, contextual)
	case FormatFLAC:
		return openFLAC(src, rs, contextual)
	case FormatMPEGTS:
		var reader io.Reader = rs
		var streamingContext *contextReader
		if !seekable {
			reader = src
			if contextSource, ok := src.(ContextSource); ok {
				streamingContext = &contextReader{source: contextSource, ctx: ctx}
				reader = streamingContext
			}
		}
		return openMPEGTS(src, reader, seekable, streamingContext)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
}

type contextReader struct {
	source ContextSource
	mu     sync.RWMutex
	ctx    context.Context
}

func (r *contextReader) Read(p []byte) (int, error) {
	r.mu.RLock()
	ctx := r.ctx
	r.mu.RUnlock()
	return r.source.ReadContext(ctx, p)
}

func (r *contextReader) setContext(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

type webMDemuxer struct {
	stateMu    sync.Mutex
	opMu       sync.Mutex
	src        Source
	rd         *webm.DemuxReader
	streams    []Stream
	trackIndex map[int]int
	info       Info
	contextual *contextReadSeeker
	closed     bool
}

func newWebMDemuxer(src Source, rd *webm.DemuxReader, contextual *contextReadSeeker) *webMDemuxer {
	tracks := rd.Tracks()
	streams := make([]Stream, 0, len(tracks))
	trackIndex := make(map[int]int, len(tracks))
	for _, track := range tracks {
		index := len(streams)
		trackIndex[track.Number] = index
		stream := Stream{
			Index:         index,
			ID:            int64(track.Number),
			Type:          MediaAudio,
			Codec:         codecID(track.Codec),
			TimeBase:      nanosecondTimeBase,
			Language:      track.Language,
			Metadata:      map[string]string{"title": track.Name},
			SampleRate:    int(math.Round(track.SampleRate)),
			Channels:      track.Channels,
			Width:         track.Width,
			Height:        track.Height,
			CodecDelay:    time.Duration(track.CodecDelayNS),
			SeekPreRoll:   time.Duration(track.SeekPreRollNS),
			DefaultPacket: time.Duration(track.DefaultDurationNS),
		}
		if track.IsVideo {
			stream.Type = MediaVideo
		}
		if track.Default {
			stream.Disposition |= DispositionDefault
		}
		stream.Config.Data = append([]byte(nil), track.CodecPrivate...)
		if stream.Codec == CodecOpus {
			stream.Config.Format = CodecConfigOpusHead
		}
		streams = append(streams, stream)
	}
	metadata := rd.Metadata()
	format := FormatMatroska
	if strings.EqualFold(metadata.DocType, "webm") {
		format = FormatWebM
	}
	info := Info{
		Format:        format,
		FormatName:    metadata.DocType,
		Duration:      metadata.Duration,
		DurationKnown: metadata.DurationKnown,
		Metadata:      metadata.Tags,
	}
	return &webMDemuxer{src: src, rd: rd, streams: streams, trackIndex: trackIndex, info: info, contextual: contextual}
}

func (d *webMDemuxer) Streams() []Stream {
	return cloneStreams(d.streams)
}

func (d *webMDemuxer) Info() Info {
	info := d.info
	info.Metadata = cloneMetadata(info.Metadata)
	return info
}

func (d *webMDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil, ErrClosed
	}
	d.stateMu.Unlock()
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	p, err := d.rd.NextPacket(ctx)
	if err != nil {
		return nil, err
	}
	index, ok := d.trackIndex[p.TrackNum]
	if !ok {
		return nil, fmt.Errorf("%w: WebM track %d", ErrInvalidData, p.TrackNum)
	}
	flags := PacketFlags(0)
	if p.Keyframe {
		flags |= PacketKeyframe
	}
	data := append([]byte(nil), p.Data...)
	return &Packet{
		StreamIndex:    index,
		Data:           data,
		PTS:            KnownTimestamp(p.TimestampNS),
		DTS:            KnownTimestamp(p.TimestampNS),
		Duration:       KnownTimestamp(p.DurationNS),
		Flags:          flags,
		Pos:            p.Position,
		DiscardPadding: time.Duration(p.DiscardPaddingNS),
	}, nil
}

func (d *webMDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return SeekResult{}, ErrClosed
	}
	d.stateMu.Unlock()
	if err := validateSeekRequest(req, len(d.streams)); err != nil {
		return SeekResult{}, err
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	targetNS := req.Target
	trackNumber := -1
	if req.StreamIndex >= 0 {
		var ok bool
		targetNS, ok = d.streams[req.StreamIndex].TimeBase.Rescale(req.Target, nanosecondTimeBase)
		if !ok {
			return SeekResult{}, errors.New("media: seek timestamp overflow")
		}
		trackNumber = int(d.streams[req.StreamIndex].ID)
	}
	if targetNS < 0 {
		targetNS = 0
	}
	metadata := d.rd.Metadata()
	if metadata.TimestampScaleNS == 0 {
		return SeekResult{}, fmt.Errorf("%w: zero WebM timestamp scale", ErrInvalidData)
	}
	ticks := uint64(targetNS) / metadata.TimestampScaleNS
	actualTicks, err := d.rd.SeekTicksWithFlags(ctx, ticks, trackNumber, req.Flags&SeekBackward != 0, req.Flags&SeekAny != 0)
	if err != nil {
		return SeekResult{}, err
	}
	if actualTicks > math.MaxInt64/metadata.TimestampScaleNS {
		return SeekResult{}, errors.New("media: seek result overflow")
	}
	actualNS := int64(actualTicks * metadata.TimestampScaleNS)
	result := SeekResult{StreamIndex: req.StreamIndex, Timestamp: actualNS}
	if req.StreamIndex >= 0 {
		var ok bool
		result.Timestamp, ok = nanosecondTimeBase.Rescale(actualNS, d.streams[req.StreamIndex].TimeBase)
		if !ok {
			return SeekResult{}, errors.New("media: seek result overflow")
		}
	}
	return result, nil
}

type contextReadSeeker struct {
	io.ReadSeeker
	source ContextSource
	mu     sync.RWMutex
	ctx    context.Context
}

func (r *contextReadSeeker) Read(p []byte) (int, error) {
	r.mu.RLock()
	ctx := r.ctx
	r.mu.RUnlock()
	return r.source.ReadContext(ctx, p)
}

func (r *contextReadSeeker) setContext(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

func (d *webMDemuxer) Close() error {
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.stateMu.Unlock()
	sourceErr := d.src.Close()
	d.opMu.Lock()
	readerErr := d.rd.Close()
	d.opMu.Unlock()
	return errors.Join(sourceErr, readerErr)
}

func codecID(codec core.CodecType) CodecID {
	switch codec {
	case core.CodecVP8:
		return CodecVP8
	case core.CodecVP9:
		return CodecVP9
	case core.CodecAV1:
		return CodecAV1
	case core.CodecOpus:
		return CodecOpus
	case core.CodecVorbis:
		return CodecVorbis
	case core.CodecFLAC:
		return CodecFLAC
	case core.CodecAAC:
		return CodecAAC
	case core.CodecMP3:
		return CodecMP3
	case core.CodecH264:
		return CodecH264
	case core.CodecHEVC:
		return CodecHEVC
	default:
		return CodecUnknown
	}
}

func cloneStreams(in []Stream) []Stream {
	out := make([]Stream, len(in))
	copy(out, in)
	for i := range out {
		out[i].Metadata = cloneMetadata(out[i].Metadata)
		out[i].Config.Data = append([]byte(nil), out[i].Config.Data...)
	}
	return out
}

func cloneMetadata(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
