package puremux

import (
	"fmt"
	"io"

	"github.com/famomatic/puremux/internal/format/mp4"
)

// mp4ReaderAdapter wraps internal/format/mp4.Reader so it satisfies
// inputReader. It translates mp4.Sample into InputBlock. The MP4 demuxer
// is provided in internal/format/mp4 (Phase C); this adapter is the bridge
// into the container-agnostic merge loop.
type mp4ReaderAdapter struct {
	f interface {
		io.Reader
		io.Closer
	}
	rd     *mp4.Reader
	tracks []InputTrack
}

func newMP4Reader(f interface {
	io.Reader
	io.Closer
}) (inputReader, error) {
	rd, err := mp4.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("puremux: read mp4: %w", err)
	}
	a := &mp4ReaderAdapter{f: f, rd: rd}
	for _, t := range rd.Tracks() {
		a.tracks = append(a.tracks, InputTrack{
			Number:     t.Number,
			Codec:      t.Codec,
			IsVideo:    t.IsVideo,
			Width:      t.Width,
			Height:     t.Height,
			Channels:   t.Channels,
			SampleRate: t.SampleRate,
		})
	}
	return a, nil
}

func (a *mp4ReaderAdapter) Tracks() []InputTrack { return a.tracks }

func (a *mp4ReaderAdapter) NextBlock() (*InputBlock, error) {
	blk, err := a.rd.NextSample()
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}
	return &InputBlock{
		TrackNum: blk.TrackNum,
		AbsMs:    blk.AbsMs,
		Keyframe: blk.Keyframe,
		Data:     blk.Data,
	}, nil
}

func (a *mp4ReaderAdapter) Close() error { return a.f.Close() }
