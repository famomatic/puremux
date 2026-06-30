package puremux

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/internal/format/webm"
)

// openFile opens an input file read-only.
func openFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("puremux: open %s: %w", path, err)
	}
	return f, nil
}

// webmReaderAdapter wraps internal/format/webm.Reader so it satisfies
// inputReader. It translates webm.Block into InputBlock.
type webmReaderAdapter struct {
	f      *os.File
	rd     *webm.Reader
	tracks []InputTrack
}

func newWebMReader(f *os.File) (inputReader, error) {
	rd, err := webm.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("puremux: read webm: %w", err)
	}
	a := &webmReaderAdapter{f: f, rd: rd}
	for _, t := range rd.Tracks() {
		a.tracks = append(a.tracks, InputTrack{
			Number:       t.Number,
			Codec:        t.Codec,
			IsVideo:      t.IsVideo,
			Width:        t.Width,
			Height:       t.Height,
			Channels:     t.Channels,
			SampleRate:   t.SampleRate,
			CodecPrivate: t.CodecPrivate,
		})
	}
	return a, nil
}

func (a *webmReaderAdapter) Tracks() []InputTrack { return a.tracks }

func (a *webmReaderAdapter) NextBlock() (*InputBlock, error) {
	blk, err := a.rd.NextBlock()
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}
	return &InputBlock{
		TrackNum: blk.TrackNum,
		AbsMs:    blk.AbsTimecode(),
		Keyframe: blk.Keyframe,
		Data:     blk.Data,
	}, nil
}

func (a *webmReaderAdapter) Close() error { return a.f.Close() }

// RemuxInputs merges one or more EBML (WebM/MKV) input files into a single
// WebM/MKV output via the puremux pipeline. This is the legacy entry point
// that assumes all inputs are EBML; new callers should use Merge, which gates
// on the output container and input magic first.
//
// Each input is demuxed (opaque payloads, no decoding), fed through the
// Enforcer+Aligner pipeline, and serialized to the output. Packets are
// emitted in merged timecode order across all inputs.
func RemuxInputs(inputs []string, w io.Writer, cfg Config) error {
	containers := make([]Container, len(inputs))
	for i, in := range inputs {
		c, err := DetectContainer(in)
		if err != nil {
			return fmt.Errorf("puremux: detect %s: %w", in, err)
		}
		containers[i] = c
	}
	return remuxInputs(inputs, containers, w, cfg)
}

// remuxInputs is the container-agnostic merge core. It opens each input with
// the matching demuxer, registers tracks on a fresh session, and drains all
// sources in merged absolute-timecode order. The cfg.OutputContainer decides
// the EBML doctype written; it defaults to WebM when zero.
func remuxInputs(inputs []string, containers []Container, w io.Writer, cfg Config) error {
	if len(inputs) == 0 {
		return fmt.Errorf("puremux: no inputs")
	}
	if cfg.OutputContainer == ContainerUnknown {
		cfg.OutputContainer = ContainerWebM
	}

	type source struct {
		reader inputReader
		tracks []InputTrack
		next   *InputBlock
	}
	var srcs []*source
	trackIDMap := map[int]int{} // (srcIdx, srcTrackNum) -> output track number

	for _, path := range inputs {
		_ = path
	}

	// Open all inputs.
	for i, path := range inputs {
		rd, err := openInputReader(path, containers[i])
		if err != nil {
			for _, s := range srcs {
				_ = s.reader.Close()
			}
			return err
		}
		srcs = append(srcs, &source{reader: rd, tracks: rd.Tracks()})
	}
	defer func() {
		for _, s := range srcs {
			_ = s.reader.Close()
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
	for _, src := range srcs {
		blk, err := src.reader.NextBlock()
		if err == io.EOF {
			src.next = nil
			continue
		}
		if err != nil {
			return fmt.Errorf("puremux: read block: %w", err)
		}
		src.next = blk
	}

	// Merge loop: pick the source with the earliest block, emit it, advance.
	for {
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
			break
		}
		src := srcs[pick]
		blk := src.next
		outTrack := trackIDMap[pick*1000+blk.TrackNum]
		p := &core.Packet{
			TrackID:    outTrack,
			DTS:        blk.Duration(),
			PTS:        blk.Duration(),
			IsKeyframe: blk.Keyframe,
			Codec:      trackCodec(src.tracks, blk.TrackNum),
			Data:       blk.Data,
		}
		if err := s.WritePacket(p); err != nil {
			return fmt.Errorf("puremux: write packet: %w", err)
		}
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
func mergeByTime(blocks []*InputBlock) {
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].AbsTimecode() < blocks[j].AbsTimecode()
	})
}
