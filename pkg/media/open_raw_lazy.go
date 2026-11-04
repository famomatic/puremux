package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
	"github.com/famomatic/puremux/pkg/bitstream/mp3"
)

type lazyAudioDemuxer struct {
	base             *indexedAudioDemuxer
	indexed          *indexedAudioDemuxer
	offset, end, pts int64
	number           uint64
	flacInfo         flac.StreamInfo
}

func newLazyAudio(src Source, rs io.ReadSeeker, ctx *contextReadSeeker, format Format, codec CodecID, rate, channels int, config []byte, configFormat CodecConfigFormat, metadata map[string]string, start, end int64) *lazyAudioDemuxer {
	return &lazyAudioDemuxer{base: newIndexedAudio(src, rs, format, codec, rate, channels, config, configFormat, nil, metadata, ctx), offset: start, end: end}
}
func (d *lazyAudioDemuxer) Streams() []Stream {
	d.base.opMu.Lock()
	defer d.base.opMu.Unlock()
	if d.indexed != nil {
		return d.indexed.Streams()
	}
	return d.base.Streams()
}
func (d *lazyAudioDemuxer) Info() Info {
	d.base.opMu.Lock()
	defer d.base.opMu.Unlock()
	if d.indexed != nil {
		return d.indexed.Info()
	}
	return d.base.Info()
}
func (d *lazyAudioDemuxer) Close() error { return d.base.Close() }
func (d *lazyAudioDemuxer) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.base.stateMu.Lock()
	closed := d.base.closed
	d.base.stateMu.Unlock()
	if closed {
		return ErrClosed
	}
	if d.base.contextual != nil {
		d.base.contextual.setContext(ctx)
	}
	return nil
}
func (d *lazyAudioDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	if err := d.base.opMu.lock(ctx); err != nil {
		return nil, err
	}
	defer d.base.opMu.Unlock()
	if err := d.ready(ctx); err != nil {
		return nil, err
	}
	if d.indexed != nil {
		return d.indexed.ReadPacket(ctx)
	}
	if d.offset >= d.end {
		d.finish()
		return nil, io.EOF
	}
	if _, err := d.base.rs.Seek(d.offset, io.SeekStart); err != nil {
		return nil, err
	}
	var data []byte
	var duration int64
	position := d.offset
	next := d.offset
	switch d.base.info.Format {
	case FormatADTS:
		var h [7]byte
		if _, err := io.ReadFull(d.base.rs, h[:]); err != nil {
			return nil, err
		}
		frame, ok := core.ParseADTSHeader(h[:])
		headerLen := 7
		if h[1]&1 == 0 {
			headerLen = 9
		}
		if !ok || frame.Length < headerLen || int64(frame.Length) > d.end-d.offset {
			return nil, ErrInvalidData
		}
		config, err := aac.ASC(aac.Config{AudioObjectType: int(h[2]>>6) + 1, SampleRate: frame.SampleRate, FrequencyIndex: int(h[2]>>2) & 15, ChannelConfig: frame.ChannelConfig})
		if err != nil || !bytes.Equal(config, d.base.stream.Config.Data) {
			return nil, ErrInvalidData
		}
		if headerLen == 9 {
			var crc [2]byte
			if _, err := io.ReadFull(d.base.rs, crc[:]); err != nil {
				return nil, err
			}
		}
		data = core.AcquirePayload(frame.Length - headerLen)
		if _, err := io.ReadFull(d.base.rs, data); err != nil {
			core.ReleasePayload(data)
			return nil, err
		}
		position += int64(headerLen)
		next += int64(frame.Length)
		duration = int64(frame.Samples)
	case FormatMP3:
		var h [4]byte
		if _, err := io.ReadFull(d.base.rs, h[:]); err != nil {
			return nil, err
		}
		if d.end-d.offset == 128 && string(h[:3]) == "TAG" {
			_, tags, err := readID3v1(d.base.rs, d.end, d.offset)
			if err != nil {
				return nil, err
			}
			if d.base.info.Metadata == nil {
				d.base.info.Metadata = make(map[string]string)
			}
			if d.base.stream.Metadata == nil {
				d.base.stream.Metadata = make(map[string]string)
			}
			for k, v := range tags {
				if d.base.info.Metadata[k] == "" {
					d.base.info.Metadata[k] = v
					d.base.stream.Metadata[k] = v
				}
			}
			d.offset = d.end
			d.finish()
			return nil, io.EOF
		}
		frame, err := mp3.ParseHeader(h[:])
		if err != nil || int64(frame.FrameLength) > d.end-d.offset || frame.SampleRate != d.base.stream.SampleRate || frame.Channels != d.base.stream.Channels {
			return nil, ErrInvalidData
		}
		data = core.AcquirePayload(frame.FrameLength)
		copy(data, h[:])
		if _, err := io.ReadFull(d.base.rs, data[4:]); err != nil {
			core.ReleasePayload(data)
			return nil, err
		}
		next += int64(frame.FrameLength)
		duration = int64(frame.Samples)
	case FormatFLAC:
		var err error
		data, duration, err = d.readFLACFrame()
		if err != nil {
			return nil, err
		}
		next += int64(len(data))
	}
	if duration <= 0 || d.pts > math.MaxInt64-duration {
		core.ReleasePayload(data)
		return nil, ErrInvalidData
	}
	p := &Packet{StreamIndex: 0, Data: data, PTS: KnownTimestamp(d.pts), DTS: KnownTimestamp(d.pts), Duration: KnownTimestamp(duration), Flags: PacketKeyframe, Pos: position}
	if d.base.info.Format != FormatFLAC {
		p.release = core.ReleasePayload
	}
	d.offset = next
	d.pts += duration
	d.number++
	return p, nil
}
func (d *lazyAudioDemuxer) finish() {
	d.base.stream.Duration = KnownTimestamp(d.pts)
	d.base.info.Duration, d.base.info.DurationKnown = d.base.stream.TimeBase.Duration(d.pts)
}
func (d *lazyAudioDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	if err := d.base.opMu.lock(ctx); err != nil {
		return SeekResult{}, err
	}
	defer d.base.opMu.Unlock()
	if err := d.ready(ctx); err != nil {
		return SeekResult{}, err
	}
	if err := validateSeekRequest(req, 1); err != nil {
		return SeekResult{}, err
	}
	if d.indexed != nil {
		return d.indexed.Seek(ctx, req)
	}
	pos, err := d.base.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return SeekResult{}, err
	}
	var candidate Demuxer
	switch d.base.info.Format {
	case FormatADTS:
		candidate, err = openADTS(d.base.src, d.base.rs, d.base.contextual)
	case FormatMP3:
		candidate, err = openMP3(d.base.src, d.base.rs, d.base.contextual)
	case FormatFLAC:
		candidate, err = openFLAC(d.base.src, d.base.rs, d.base.contextual)
	}
	_, restoreErr := d.base.rs.Seek(pos, io.SeekStart)
	if err = errors.Join(err, restoreErr); err != nil {
		return SeekResult{}, err
	}
	result, err := candidate.Seek(ctx, req)
	if err != nil {
		return SeekResult{}, err
	}
	d.indexed = candidate.(*indexedAudioDemuxer)
	return result, nil
}
func (d *lazyAudioDemuxer) readFLACFrame() ([]byte, int64, error) {
	r := bufio.NewReaderSize(d.base.rs, 64<<10)
	head, err := r.Peek(int(min(16, d.end-d.offset)))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	h, err := flac.ParseFrameHeader(head, d.flacInfo)
	if err != nil {
		return nil, 0, err
	}
	expected := d.number
	if h.VariableBlock {
		expected = uint64(d.pts)
	}
	if h.Number != expected || h.SampleRate != d.flacInfo.SampleRate || h.Channels != d.flacInfo.Channels || h.BitsPerSample != d.flacInfo.BitsPerSample {
		return nil, 0, ErrInvalidData
	}
	var data []byte
	var crc uint16
	for d.offset+int64(len(data)) < d.end {
		if len(data) >= h.HeaderLength+2 && crc == 0 {
			candidate, _ := r.Peek(int(min(16, d.end-d.offset-int64(len(data)))))
			next, e := flac.ParseFrameHeader(candidate, d.flacInfo)
			number := d.number + 1
			if h.VariableBlock {
				number = uint64(d.pts) + uint64(h.BlockSize)
			}
			if e == nil && next.Number == number && next.VariableBlock == h.VariableBlock && next.SampleRate == h.SampleRate && next.Channels == h.Channels && next.BitsPerSample == h.BitsPerSample {
				return data, int64(h.BlockSize), nil
			}
		}
		if len(data) >= 64<<20 {
			return nil, 0, ErrInvalidData
		}
		b, err := r.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		data = append(data, b)
		crc = flac.UpdateCRC16(crc, []byte{b})
	}
	if crc != 0 || len(data) < h.HeaderLength+2 {
		return nil, 0, ErrInvalidData
	}
	return data, int64(h.BlockSize), nil
}
