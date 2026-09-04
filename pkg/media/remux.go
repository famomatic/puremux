package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RemuxInput pairs an input source with its demuxer options. Remux takes
// ownership of every non-nil Source and closes it before returning, including
// sources it cannot open.
type RemuxInput struct {
	Source  Source
	Options OpenOptions
}

// Remux copies compressed packets from one or more inputs into a new
// container. Streams are registered in input order and packets are merged by
// exact DTS; equal timestamps retain input order. The destination remains
// owned by the caller.
func Remux(ctx context.Context, inputs []RemuxInput, dst io.Writer, opts MuxOptions) error {
	if err := ctx.Err(); err != nil {
		closeRemuxSources(inputs)
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("%w: no remux inputs", ErrInvalidData)
	}
	demuxers := make([]Demuxer, 0, len(inputs))
	defer func() {
		for _, demuxer := range demuxers {
			_ = demuxer.Close()
		}
		for _, input := range inputs[len(demuxers):] {
			if input.Source != nil {
				_ = input.Source.Close()
			}
		}
	}()
	for i, input := range inputs {
		if input.Source == nil {
			return fmt.Errorf("%w: nil remux input %d", ErrInvalidData, i)
		}
		demuxer, err := Open(ctx, input.Source, input.Options)
		if err != nil {
			return fmt.Errorf("media: open input %d: %w", i, err)
		}
		demuxers = append(demuxers, demuxer)
	}
	muxer, err := NewMuxer(dst, opts)
	if err != nil {
		return err
	}
	return remuxDemuxers(ctx, demuxers, muxer, opts.Format)
}

func closeRemuxSources(inputs []RemuxInput) {
	for _, input := range inputs {
		if input.Source != nil {
			_ = input.Source.Close()
		}
	}
}

type remuxCursor struct {
	demuxer Demuxer
	streams []Stream
	mapping []int
	next    *Packet
	done    bool
}

func remuxDemuxers(ctx context.Context, demuxers []Demuxer, muxer Muxer, output Format) (retErr error) {
	closed := false
	defer func() {
		if !closed {
			if err := muxer.Close(); retErr == nil {
				retErr = err
			}
		}
	}()
	cursors := make([]remuxCursor, len(demuxers))
	defer func() {
		for i := range cursors {
			if cursors[i].next != nil {
				cursors[i].next.Release()
				cursors[i].next = nil
			}
		}
	}()
	for i, demuxer := range demuxers {
		if demuxer == nil {
			return fmt.Errorf("%w: nil demuxer %d", ErrInvalidData, i)
		}
		streams := demuxer.Streams()
		if len(streams) == 0 {
			return fmt.Errorf("%w: input %d has no streams", ErrInvalidData, i)
		}
		cursor := remuxCursor{demuxer: demuxer, streams: streams, mapping: make([]int, len(streams))}
		for j, stream := range streams {
			if !stream.TimeBase.Valid() || stream.TimeBase.Num <= 0 {
				return fmt.Errorf("%w: input %d stream %d time base", ErrInvalidData, i, j)
			}
			if err := validateRemuxFraming(demuxer.Info().Format, output, stream); err != nil {
				return fmt.Errorf("media: input %d stream %d: %w", i, j, err)
			}
			mapped, err := muxer.AddStream(stream)
			if err != nil {
				return fmt.Errorf("media: add input %d stream %d: %w", i, j, err)
			}
			cursor.mapping[j] = mapped
		}
		cursors[i] = cursor
	}
	for i := range cursors {
		if err := readRemuxPacket(ctx, &cursors[i]); err != nil {
			return fmt.Errorf("media: read input %d: %w", i, err)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pick := -1
		for i := range cursors {
			if cursors[i].done {
				continue
			}
			if pick < 0 || compareRemuxDTS(cursors[i], cursors[pick]) < 0 {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		cursor := &cursors[pick]
		packet := cursor.next
		if packet.StreamIndex < 0 || packet.StreamIndex >= len(cursor.mapping) {
			packet.Release()
			cursor.next = nil
			return fmt.Errorf("%w: input %d packet stream index", ErrInvalidData, pick)
		}
		outputPacket := &Packet{
			StreamIndex: cursor.mapping[packet.StreamIndex], Data: packet.Data,
			PTS: packet.PTS, DTS: packet.DTS, Duration: packet.Duration,
			Flags: packet.Flags, Pos: packet.Pos, DiscardPadding: packet.DiscardPadding,
		}
		err := muxer.WritePacket(ctx, outputPacket)
		packet.Release()
		cursor.next = nil
		if err != nil {
			return fmt.Errorf("media: write input %d packet: %w", pick, err)
		}
		if err := readRemuxPacket(ctx, cursor); err != nil {
			return fmt.Errorf("media: read input %d: %w", pick, err)
		}
	}
	closed = true
	if err := muxer.Close(); err != nil {
		return fmt.Errorf("media: finalize output: %w", err)
	}
	return nil
}

func readRemuxPacket(ctx context.Context, cursor *remuxCursor) error {
	packet, err := cursor.demuxer.ReadPacket(ctx)
	if errors.Is(err, io.EOF) {
		cursor.done = true
		return nil
	}
	if err != nil {
		return err
	}
	if packet == nil || packet.StreamIndex < 0 || packet.StreamIndex >= len(cursor.streams) ||
		!packet.DTS.Valid || !packet.PTS.Valid || !packet.Duration.Valid {
		if packet != nil {
			packet.Release()
		}
		return fmt.Errorf("%w: packet timing or stream", ErrInvalidData)
	}
	cursor.next = packet
	return nil
}

func compareRemuxDTS(a, b remuxCursor) int {
	aValue := a.next.DTS.Value
	bValue := b.next.DTS.Value
	if aValue < 0 && bValue >= 0 {
		return -1
	}
	if aValue >= 0 && bValue < 0 {
		return 1
	}
	aTB := a.streams[a.next.StreamIndex].TimeBase
	bTB := b.streams[b.next.StreamIndex].TimeBase
	left := mul192(unsignedMagnitude(aValue), uint64(aTB.Num), uint64(bTB.Den))
	right := mul192(unsignedMagnitude(bValue), uint64(bTB.Num), uint64(aTB.Den))
	comparison := compare192(left, right)
	if aValue < 0 {
		return -comparison
	}
	return comparison
}

func compare192(a, b [3]uint64) int {
	for i := 2; i >= 0; i-- {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func validateRemuxFraming(input, output Format, stream Stream) error {
	if output != FormatMPEGTS {
		return nil
	}
	if input == FormatMPEGTS && (stream.Codec == CodecH264 || stream.Codec == CodecHEVC) {
		return nil
	}
	return fmt.Errorf("%w: %s/%s packets are not MPEG-TS Annex-B/ADTS framing", ErrIncompatible, input, stream.Codec)
}

// RemuxFiles atomically remuxes input paths to outputPath. When opts.Format is
// unknown it is inferred from the output extension. Existing output is kept
// intact if remuxing or installation fails.
func RemuxFiles(ctx context.Context, inputPaths []string, outputPath string, opts MuxOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(inputPaths) == 0 || outputPath == "" {
		return fmt.Errorf("%w: missing input or output path", ErrInvalidData)
	}
	if opts.Format == FormatUnknown {
		format, err := outputFormatForPath(outputPath)
		if err != nil {
			return err
		}
		opts.Format = format
	}
	if err := rejectRemuxAlias(inputPaths, outputPath); err != nil {
		return err
	}
	inputs := make([]RemuxInput, 0, len(inputPaths))
	for _, path := range inputPaths {
		source, err := OpenFile(path)
		if err != nil {
			closeRemuxSources(inputs)
			return fmt.Errorf("media: open %s: %w", path, err)
		}
		inputs = append(inputs, RemuxInput{Source: source})
	}
	temporary, err := os.CreateTemp(filepath.Dir(outputPath), ".puremux-*.tmp")
	if err != nil {
		closeRemuxSources(inputs)
		return fmt.Errorf("media: create output %s: %w", outputPath, err)
	}
	temporaryPath := temporary.Name()
	installed := false
	defer func() {
		_ = temporary.Close()
		if !installed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		closeRemuxSources(inputs)
		return err
	}
	if err := Remux(ctx, inputs, temporary, opts); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("media: close output %s: %w", outputPath, err)
	}
	if err := installRemuxOutput(temporaryPath, outputPath); err != nil {
		return fmt.Errorf("media: install output %s: %w", outputPath, err)
	}
	installed = true
	return nil
}

func outputFormatForPath(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".webm":
		return FormatWebM, nil
	case ".mkv", ".mka":
		return FormatMatroska, nil
	case ".mp4", ".m4a", ".m4v":
		return FormatMP4, nil
	case ".ts", ".m2ts":
		return FormatMPEGTS, nil
	default:
		return FormatUnknown, fmt.Errorf("%w: output extension %s", ErrUnsupportedFormat, filepath.Ext(path))
	}
}

func rejectRemuxAlias(inputs []string, output string) error {
	outputInfo, err := os.Stat(output)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, input := range inputs {
		inputInfo, err := os.Stat(input)
		if err != nil {
			return err
		}
		if os.SameFile(inputInfo, outputInfo) {
			return fmt.Errorf("%w: output aliases input %s", ErrInvalidData, input)
		}
	}
	return nil
}

func installRemuxOutput(temporaryPath, outputPath string) error {
	if err := os.Rename(temporaryPath, outputPath); err == nil {
		return nil
	}
	if _, err := os.Stat(outputPath); err != nil {
		return err
	}
	backup, err := os.CreateTemp(filepath.Dir(outputPath), ".puremux-backup-*.tmp")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(outputPath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		_ = os.Rename(backupPath, outputPath)
		return err
	}
	return os.Remove(backupPath)
}
