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
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("puremux: open output %s: %w", outputPath, err)
	}
	defer f.Close()

	// Pass the chosen output container down through the config so the session
	// writes the correct EBML doctype and codec set. RemuxInputs re-derives
	// tracks from the inputs; the container only affects header writing.
	cfg.OutputContainer = out

	if err := remuxInputs(inputs, inContainers, f, cfg); err != nil {
		_ = os.Remove(outputPath)
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
	return remuxInputs(inputs, inContainers, w, cfg)
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
