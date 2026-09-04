package puremux

import (
	"fmt"
	"io"
	"math"
	"time"

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
			Number:       t.Number,
			Codec:        t.Codec,
			IsVideo:      t.IsVideo,
			Width:        t.Width,
			Height:       t.Height,
			Channels:     t.Channels,
			SampleRate:   t.SampleRate,
			CodecPrivate: append([]byte(nil), t.CodecConfig...),
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
	pts, okPTS := mp4TicksToDuration(blk.PTS, blk.Timescale)
	dts, okDTS := mp4TicksToDuration(blk.DTS, blk.Timescale)
	if !okPTS || !okDTS {
		return nil, fmt.Errorf("puremux: MP4 sample timestamp overflow")
	}
	return &InputBlock{
		TrackNum:    blk.TrackNum,
		AbsMs:       blk.AbsMs,
		Keyframe:    blk.Keyframe,
		Data:        blk.Data,
		PTS:         pts,
		DTS:         dts,
		ExactTiming: true,
	}, nil
}

func mp4TicksToDuration(ticks int64, scale uint32) (time.Duration, bool) {
	if scale == 0 {
		return 0, false
	}
	den := int64(scale)
	whole, remainder := ticks/den, ticks%den
	if whole > math.MaxInt64/int64(time.Second) || whole < math.MinInt64/int64(time.Second) {
		return 0, false
	}
	return time.Duration(whole*int64(time.Second) + remainder*int64(time.Second)/den), true
}

func (a *mp4ReaderAdapter) Close() error { return a.f.Close() }
