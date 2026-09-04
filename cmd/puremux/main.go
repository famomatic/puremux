// Command puremux probes and remuxes compressed media without decoding it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/famomatic/puremux/pkg/puremux"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		writeUsage(stdout)
		return 0
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "merge":
		return runMerge(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "puremux: unknown command %q\n", args[0])
		writeUsage(stderr)
		return 2
	}
}

func runProbe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, "Usage: puremux probe INPUT") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	info, err := puremux.Probe(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "puremux: probe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "container: %s\n", info.Container)
	for _, track := range info.Tracks {
		kind := "unknown"
		switch track.Kind {
		case puremux.TrackVideo:
			kind = "video"
		case puremux.TrackAudio:
			kind = "audio"
		}
		fmt.Fprintf(stdout, "track %d: %s %s\n", track.Number, kind, track.Codec)
	}
	return 0
}

func runMerge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("merge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var output string
	flags.StringVar(&output, "o", "", "output .webm or .mkv path")
	flags.StringVar(&output, "output", "", "output .webm or .mkv path")
	flags.Usage = func() { fmt.Fprintln(stderr, "Usage: puremux merge -o OUTPUT INPUT [INPUT ...]") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if output == "" || flags.NArg() == 0 {
		flags.Usage()
		return 2
	}
	if err := puremux.Merge(ctx, flags.Args(), output, puremux.DefaultConfig()); err != nil {
		fmt.Fprintf(stderr, "puremux: merge: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s from %s\n", output, strings.Join(flags.Args(), ", "))
	return 0
}

func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "puremux: pure-Go compressed-media remuxer")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  puremux probe INPUT")
	fmt.Fprintln(w, "  puremux merge -o OUTPUT.webm INPUT [INPUT ...]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Output may be WebM (.webm) or Matroska (.mkv/.mka).")
}
