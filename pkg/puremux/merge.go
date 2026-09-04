package puremux

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Merge remuxes one or more input media files into a single output container.
//
// It is the path-based entry point a FallbackMuxer calls: it performs the
// cheap container/codec compatibility gate up front (output extension + input
// magic sniff) and only invokes the heavy RemuxInputs pipeline when the gate
// passes. On an unsupported input or output it returns a sentinel error
// (ErrUnsupportedInput / ErrUnsupportedOutput / ErrIncompatible) so the caller
// can fall back to an external muxer without paying for a failed deep parse.
//
// The output container is chosen from the output file extension:
//   - .webm -> WebM
//   - .mkv  -> Matroska (MKV)
//
// Any other extension yields ErrUnsupportedOutput.
//
// ctx is currently used only for cancellation of long merges; the underlying
// readers check it opportunistically.
func Merge(ctx context.Context, inputs []string, outputPath string, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("puremux: no inputs")
	}
	if outputPath == "" {
		return fmt.Errorf("puremux: empty output path")
	}

	out, err := outputContainerForPath(outputPath)
	if err != nil {
		return err
	}

	// Output container must be writable by puremux.
	if !isWritableOutput(out) {
		return fmt.Errorf("%w: %s", ErrUnsupportedOutput, out)
	}
	// MPEG-TS is Session-only output: file inputs carry AVCC-framed video and
	// raw AAC, and puremux does not convert bitstream framing (§4 opacity).
	if out == ContainerMPEGTS {
		return fmt.Errorf("%w: %s (live Session API only)", ErrUnsupportedOutput, out)
	}

	// Sniff each input and verify the (input, output) pair is remuxable.
	inContainers := make([]Container, len(inputs))
	for i, in := range inputs {
		c, derr := DetectContainer(in)
		if derr != nil {
			return fmt.Errorf("puremux: detect %s: %w", in, derr)
		}
		if !isReadableInput(c) {
			return fmt.Errorf("%w: %s (%s)", ErrUnsupportedInput, in, c)
		}
		if !CanRemux(c, out) {
			return fmt.Errorf("%w: %s -> %s", ErrIncompatible, c, out)
		}
		inContainers[i] = c
	}

	// Open the output writer.
	if err := rejectAliasedOutput(inputs, outputPath); err != nil {
		return err
	}
	dir := filepath.Dir(outputPath)
	f, err := os.CreateTemp(dir, ".puremux-*.tmp")
	if err != nil {
		return fmt.Errorf("puremux: open output %s: %w", outputPath, err)
	}
	tempPath := f.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return fmt.Errorf("puremux: set output permissions: %w", err)
	}

	// Pass the chosen output container down through the config so the session
	// writes the correct EBML doctype and codec set. RemuxInputs re-derives
	// tracks from the inputs; the container only affects header writing.
	cfg.OutputContainer = out

	if rerr := remuxInputs(ctx, inputs, inContainers, f, cfg); rerr != nil {
		// Close before removing: Windows refuses to unlink an open file, so a
		// deferred close would leave a partial output on disk.
		_ = f.Close()
		return rerr
	}
	// A close-time flush error (disk full, network FS) means the output is
	// truncated; surface it as a failure and remove the partial file rather
	// than reporting success on a bad file.
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("puremux: close output %s: %w", outputPath, cerr)
	}
	if err := replaceOutput(tempPath, outputPath); err != nil {
		return fmt.Errorf("puremux: install output %s: %w", outputPath, err)
	}
	keepTemp = true
	return nil
}

func rejectAliasedOutput(inputs []string, outputPath string) error {
	outInfo, err := os.Stat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("puremux: stat output %s: %w", outputPath, err)
	}
	for _, input := range inputs {
		inInfo, err := os.Stat(input)
		if err != nil {
			return fmt.Errorf("puremux: stat input %s: %w", input, err)
		}
		if os.SameFile(inInfo, outInfo) {
			return fmt.Errorf("puremux: output aliases input %s", input)
		}
	}
	return nil
}

// replaceOutput preserves any existing destination if installation fails.
// os.Rename is atomic when the platform permits replacing an existing file;
// Windows needs the backup fallback because Rename does not replace files.
func replaceOutput(tempPath, outputPath string) error {
	if err := os.Rename(tempPath, outputPath); err == nil {
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
	if err := os.Rename(tempPath, outputPath); err != nil {
		_ = os.Rename(backupPath, outputPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	return nil
}

// MergeToWriter is like Merge but writes to an arbitrary io.Writer instead of
// a file path. The output container must be supplied explicitly since there
// is no extension to infer it from.
func MergeToWriter(ctx context.Context, inputs []string, w io.Writer, out Container, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("puremux: no inputs")
	}
	if !isWritableOutput(out) {
		return fmt.Errorf("%w: %s", ErrUnsupportedOutput, out)
	}
	// See Merge: MPEG-TS output is reachable only through the Session live API.
	if out == ContainerMPEGTS {
		return fmt.Errorf("%w: %s (live Session API only)", ErrUnsupportedOutput, out)
	}
	inContainers := make([]Container, len(inputs))
	for i, in := range inputs {
		c, derr := DetectContainer(in)
		if derr != nil {
			return fmt.Errorf("puremux: detect %s: %w", in, derr)
		}
		if !isReadableInput(c) {
			return fmt.Errorf("%w: %s (%s)", ErrUnsupportedInput, in, c)
		}
		if !CanRemux(c, out) {
			return fmt.Errorf("%w: %s -> %s", ErrIncompatible, c, out)
		}
		inContainers[i] = c
	}
	cfg.OutputContainer = out
	return remuxInputs(ctx, inputs, inContainers, w, cfg)
}

// outputContainerForPath infers the output container from the file extension.
func outputContainerForPath(path string) (Container, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	switch ext {
	case "webm":
		return ContainerWebM, nil
	case "mkv", "mka":
		return ContainerMKV, nil
	default:
		return ContainerUnknown, fmt.Errorf("%w: extension .%s", ErrUnsupportedOutput, ext)
	}
}

// isWritableOutput reports whether puremux can mux into the container.
func isWritableOutput(c Container) bool {
	for _, o := range SupportedOutputs() {
		if o.Container == c && o.CanWrite {
			return true
		}
	}
	return false
}

// isReadableInput reports whether puremux can demux the container.
func isReadableInput(c Container) bool {
	for _, o := range SupportedInputs() {
		if o.Container == c && o.CanRead {
			return true
		}
	}
	return false
}
