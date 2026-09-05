package media

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/mpegts"
)

type mpegTSDemuxer struct {
	stateMu    sync.Mutex
	opMu       sync.Mutex
	src        Source
	rd         mpegTSInputReader
	streams    []Stream
	seekable   bool
	contextual *contextReader
	closed     bool
}

type mpegTSInputReader interface {
	Tracks() []mpegts.InputTrack
	NextPacket() (mpegts.InputPacket, error)
}

func openMPEGTS(src Source, r io.Reader, seekable bool, contextual *contextReader) (Demuxer, error) {
	var rd mpegTSInputReader
	var err error
	if seekable {
		rd, err = mpegts.NewInputReader(r)
	} else {
		rd, err = mpegts.NewStreamingInputReader(r)
	}
	if err != nil {
		return nil, errors.Join(ErrInvalidData, err)
	}
	tracks := rd.Tracks()
	streams := make([]Stream, len(tracks))
	for i, track := range tracks {
		stream := Stream{Index: i, ID: int64(track.PID), Codec: codecID(track.Codec), TimeBase: Rational{Num: 1, Den: int64(track.Timescale)}, SampleRate: track.SampleRate, Channels: track.Channels}
		switch track.Codec {
		case core.CodecAAC, core.CodecMP3:
			stream.Type, stream.Disposition = MediaAudio, DispositionDefault
		case core.CodecH264, core.CodecHEVC:
			stream.Type = MediaVideo
		}
		if track.Codec == core.CodecAAC {
			stream.Config = CodecConfig{Format: CodecConfigASC, Data: append([]byte(nil), track.Config...)}
		}
		streams[i] = stream
	}
	return &mpegTSDemuxer{src: src, rd: rd, streams: streams, seekable: seekable, contextual: contextual}, nil
}

func (d *mpegTSDemuxer) Streams() []Stream { return cloneStreams(d.streams) }
func (d *mpegTSDemuxer) Info() Info        { return Info{Format: FormatMPEGTS, FormatName: "mpegts"} }

func (d *mpegTSDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	p, err := d.rd.NextPacket()
	if err != nil {
		return nil, err
	}
	flags := PacketFlags(0)
	if p.Keyframe {
		flags |= PacketKeyframe
	}
	duration := Timestamp{}
	if p.Duration > 0 {
		duration = KnownTimestamp(p.Duration)
	}
	return &Packet{StreamIndex: p.Track, Data: p.Data, PTS: KnownTimestamp(p.PTS), DTS: KnownTimestamp(p.DTS), Duration: duration, Flags: flags, Pos: p.Offset}, nil
}

func (d *mpegTSDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return SeekResult{}, err
	}
	if err := validateSeekRequest(req, len(d.streams)); err != nil {
		return SeekResult{}, err
	}
	if !d.seekable {
		return SeekResult{}, ErrNotSeekable
	}
	index := req.StreamIndex
	if index < 0 {
		index = 0
		for i, stream := range d.streams {
			if stream.Type == MediaAudio {
				index = i
				break
			}
		}
	}
	target := req.Target
	if req.StreamIndex < 0 {
		var ok bool
		target, ok = nanosecondTimeBase.Rescale(target, d.streams[index].TimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	rd := d.rd.(*mpegts.InputReader)
	actual, err := rd.SeekWithFlags(index, target, req.Flags&SeekBackward != 0, req.Flags&SeekAny != 0)
	if err != nil {
		return SeekResult{}, err
	}
	if req.StreamIndex < 0 {
		var ok bool
		actual, ok = d.streams[index].TimeBase.Rescale(actual, nanosecondTimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
}

func (d *mpegTSDemuxer) Close() error {
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.stateMu.Unlock()
	return d.src.Close()
}

var _ Demuxer = (*mpegTSDemuxer)(nil)
