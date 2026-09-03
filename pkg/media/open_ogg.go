package media

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/format/ogg"
)

var opusTimeBase = Rational{Num: 1, Den: 48_000}

type oggDemuxer struct {
	stateMu    sync.Mutex
	opMu       sync.Mutex
	src        Source
	rd         *ogg.Reader
	stream     Stream
	info       Info
	contextual *contextReadSeeker
	closed     bool
}

func newOggDemuxer(src Source, rd *ogg.Reader, contextual *contextReadSeeker) *oggDemuxer {
	head := rd.Head()
	duration := rd.DurationSamples()
	stream := Stream{
		Index:       0,
		ID:          0,
		Type:        MediaAudio,
		Codec:       CodecOpus,
		TimeBase:    opusTimeBase,
		Disposition: DispositionDefault,
		Metadata:    rd.Tags(),
		Config: CodecConfig{
			Format: CodecConfigOpusHead,
			Data:   head.Packet,
		},
		SampleRate: 48_000,
		Channels:   int(head.Channels),
		CodecDelay: samplesDuration(int64(head.PreSkip)),
	}
	if duration > 0 {
		stream.Duration = KnownTimestamp(duration)
	}
	info := Info{Format: FormatOgg, FormatName: "ogg", Metadata: rd.Tags()}
	if duration > 0 {
		info.Duration, info.DurationKnown = samplesDuration(duration), true
	}
	return &oggDemuxer{src: src, rd: rd, stream: stream, info: info, contextual: contextual}
}

func (d *oggDemuxer) Streams() []Stream { return cloneStreams([]Stream{d.stream}) }

func (d *oggDemuxer) Info() Info {
	info := d.info
	info.Metadata = cloneMetadata(info.Metadata)
	return info
}

func (d *oggDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
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
	flags := PacketKeyframe
	data := append([]byte(nil), p.Data...)
	return &Packet{
		StreamIndex: 0,
		Data:        data,
		PTS:         KnownTimestamp(p.PTS),
		DTS:         KnownTimestamp(p.PTS),
		Duration:    KnownTimestamp(p.Duration),
		Flags:       flags,
		Pos:         p.Position,
	}, nil
}

func (d *oggDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return SeekResult{}, ErrClosed
	}
	d.stateMu.Unlock()
	if req.StreamIndex != -1 && req.StreamIndex != 0 {
		return SeekResult{}, errors.New("media: Ogg stream index out of range")
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	target := req.Target
	if req.StreamIndex == -1 {
		var ok bool
		target, ok = nanosecondTimeBase.Rescale(req.Target, opusTimeBase)
		if !ok {
			return SeekResult{}, errors.New("media: Ogg seek timestamp overflow")
		}
	}
	actual, err := d.rd.SeekSamples(ctx, target)
	if err != nil {
		return SeekResult{}, err
	}
	result := SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}
	if req.StreamIndex == -1 {
		result.Timestamp, _ = opusTimeBase.Rescale(actual, nanosecondTimeBase)
	}
	return result, nil
}

func (d *oggDemuxer) Close() error {
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

func samplesDuration(samples int64) time.Duration {
	duration, _ := opusTimeBase.Duration(samples)
	return duration
}

var _ Demuxer = (*oggDemuxer)(nil)
var _ io.Closer = (*oggDemuxer)(nil)
