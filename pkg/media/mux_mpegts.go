package media

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/mpegts"
)

type mpegtsPacketWriter interface {
	AddTrack(core.Track) (int, error)
	WritePacket(*core.Packet) error
	Close() error
}

type mpegtsMuxer struct {
	allowMetadataLoss bool
	w                 mpegtsPacketWriter
	streams           []Stream
	closed            bool
	started           bool
	closeErr          error
}

func newMPEGTSMuxer(w io.Writer) Muxer {
	return &mpegtsMuxer{w: mpegts.New(w)}
}

func (m *mpegtsMuxer) AddStream(stream Stream) (int, error) {
	if m.closed || m.started {
		return 0, errorsForMuxState(m.closed)
	}
	if !stream.TimeBase.Valid() || stream.TimeBase.Num <= 0 {
		return 0, fmt.Errorf("%w: invalid time base", ErrInvalidData)
	}
	if !m.allowMetadataLoss {
		if err := validateOutputMetadata(stream, FormatMPEGTS); err != nil {
			return 0, err
		}
	}
	codec := coreCodec(stream.Codec)
	if codec != core.CodecH264 && codec != core.CodecHEVC && codec != core.CodecAAC {
		return 0, fmt.Errorf("%w: %s in MPEG-TS", ErrUnsupportedCodec, stream.Codec)
	}
	kind := core.TrackAudio
	wantType := MediaAudio
	if codec.IsVideo() {
		kind = core.TrackVideo
		wantType = MediaVideo
	}
	if stream.Type != wantType {
		return 0, fmt.Errorf("%w: stream type does not match codec", ErrInvalidData)
	}
	index := len(m.streams)
	trackID := index + 1
	if _, err := m.w.AddTrack(core.Track{
		ID: trackID, Kind: kind, Codec: codec, Width: stream.Width,
		Height: stream.Height, Channels: stream.Channels, SampleRate: stream.SampleRate,
	}); err != nil {
		return 0, err
	}
	copyStream := cloneStreams([]Stream{stream})[0]
	copyStream.Index = index
	m.streams = append(m.streams, copyStream)
	return index, nil
}

func (m *mpegtsMuxer) WritePacket(ctx context.Context, packet *Packet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrClosed
	}
	if packet == nil {
		return nil
	}
	if packet.StreamIndex < 0 || packet.StreamIndex >= len(m.streams) ||
		!packet.PTS.Valid || !packet.DTS.Valid || (packet.Duration.Valid && packet.Duration.Value <= 0) {
		return ErrInvalidData
	}
	if packet.DiscardPadding != 0 {
		return fmt.Errorf("%w: MPEG-TS discard padding", ErrUnsupportedFormat)
	}
	stream := m.streams[packet.StreamIndex]
	pts, ok := stream.TimeBase.Duration(packet.PTS.Value)
	if !ok {
		return fmt.Errorf("%w: PTS overflow", ErrInvalidData)
	}
	dts, ok := stream.TimeBase.Duration(packet.DTS.Value)
	if !ok {
		return fmt.Errorf("%w: DTS overflow", ErrInvalidData)
	}
	m.started = true
	return m.w.WritePacket(&core.Packet{
		TrackID: packet.StreamIndex + 1,
		Codec:   coreCodec(stream.Codec), Data: packet.Data,
		PTS: time.Duration(pts), DTS: time.Duration(dts), IsKeyframe: packet.Keyframe(),
	})
}

func (m *mpegtsMuxer) Close() error {
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	m.closeErr = m.w.Close()
	return m.closeErr
}
