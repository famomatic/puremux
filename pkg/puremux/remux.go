package puremux

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/webm"
)

// RemuxInputs merges one or more WebM input files (e.g. separate video and
// audio downloads) into a single WebM output via the puremux pipeline.
//
// Each input is demuxed with the WebM reader (opaque payloads, no decoding),
// fed through the Enforcer+Aligner pipeline, and serialized to the output.
// Packets are emitted in merged timecode order across all inputs.
func RemuxInputs(inputs []string, w io.Writer, cfg Config) error {
	if len(inputs) == 0 {
		return fmt.Errorf("puremux: no inputs")
	}

	// Open all inputs and parse their tracks.
	type source struct {
		file   *os.File
		reader *webm.Reader
		tracks []webm.ReadTrack
		// next block cached per source
		next *webm.Block
		err  error
	}
	var srcs []*source
	trackIDMap := map[int]int{} // (srcIdx, srcTrackNum) -> output track number
	for _, path := range inputs {
		f, err := os.Open(path)
		if err != nil {
			for _, s := range srcs {
				s.file.Close()
			}
			return fmt.Errorf("puremux: open %s: %w", path, err)
		}
		rd, err := webm.NewReader(f)
		if err != nil {
			f.Close()
			for _, s := range srcs {
				s.file.Close()
			}
			return fmt.Errorf("puremux: read %s: %w", path, err)
		}
		srcs = append(srcs, &source{file: f, reader: rd, tracks: rd.Tracks()})
	}
	defer func() {
		for _, s := range srcs {
			s.file.Close()
		}
	}()

	// Create the output session and register tracks.
	s, err := NewSession(w, cfg)
	if err != nil {
		return err
	}
	for srcIdx, src := range srcs {
		for _, t := range src.tracks {
			num, err := s.AddTrack(Track{
				Codec:        t.Codec,
				IsVideo:      t.IsVideo,
				Width:        t.Width,
				Height:       t.Height,
				Channels:     t.Channels,
				SampleRate:   t.SampleRate,
				CodecPrivate: t.CodecPrivate,
			})
			if err != nil {
				return err
			}
			trackIDMap[srcIdx*1000+t.Number] = num
		}
	}

	// Prime each source with its first block.
	for srcIdx, src := range srcs {
		blk, err := src.reader.NextBlock()
		if err == io.EOF {
			src.next = nil
			continue
		}
		if err != nil {
			return fmt.Errorf("puremux: read block: %w", err)
		}
		src.next = blk
		_ = srcIdx
	}

	// Merge loop: pick the source with the earliest block, emit it, advance.
	for {
		// Find the source with the smallest absolute timecode.
		var pick int = -1
		var bestTC uint64
		for i, src := range srcs {
			if src.next == nil {
				continue
			}
			tc := src.next.AbsTimecode()
			if pick < 0 || tc < bestTC {
				pick = i
				bestTC = tc
			}
		}
		if pick < 0 {
			break // all sources exhausted
		}
		src := srcs[pick]
		blk := src.next
		outTrack := trackIDMap[pick*1000+blk.TrackNum]
		p := &core.Packet{
			TrackID:    outTrack,
			DTS:        blk.Duration(),
			PTS:        blk.Duration(),
			IsKeyframe: blk.Keyframe,
			Codec:      src.tracks[0].Codec, // approx; refined below
			Data:       blk.Data,
		}
		// Find the codec for this track number.
		for _, t := range src.tracks {
			if t.Number == blk.TrackNum {
				p.Codec = t.Codec
				break
			}
		}
		if err := s.WritePacket(p); err != nil {
			return fmt.Errorf("puremux: write packet: %w", err)
		}
		// Advance this source.
		blk2, err := src.reader.NextBlock()
		if err == io.EOF {
			src.next = nil
		} else if err != nil {
			return fmt.Errorf("puremux: read block: %w", err)
		} else {
			src.next = blk2
		}
	}

	if err := s.Close(); err != nil {
		return fmt.Errorf("puremux: close: %w", err)
	}
	return nil
}

// mergeByTime sorts blocks across sources by absolute timecode for stable
// merge ordering. (Currently inline above; kept for future batching.)
func mergeByTime(blocks []*webm.Block) {
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].AbsTimecode() < blocks[j].AbsTimecode()
	})
}

// Ensure time import is used (referenced via blk.Duration()).
var _ = time.Millisecond
