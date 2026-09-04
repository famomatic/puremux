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

	"github.com/famomatic/puremux/pkg/media"
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
		return runProbe(ctx, args[1:], stdout, stderr)
	case "remux":
		return runRemux(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "puremux: unknown command %q\n", args[0])
		writeUsage(stderr)
		return 2
	}
}

func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
	source, err := media.OpenFile(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "puremux: probe: %v\n", err)
		return 1
	}
	demuxer, err := media.Open(ctx, source, media.OpenOptions{})
	if err != nil {
		_ = source.Close()
		fmt.Fprintf(stderr, "puremux: probe: %v\n", err)
		return 1
	}
	defer demuxer.Close()
	fmt.Fprintf(stdout, "container: %s\n", demuxer.Info().Format)
	for _, track := range demuxer.Streams() {
		kind := "unknown"
		switch track.Type {
		case media.MediaVideo:
			kind = "video"
		case media.MediaAudio:
			kind = "audio"
		}
		fmt.Fprintf(stdout, "stream %d: %s %s\n", track.Index, kind, track.Codec)
	}
	return 0
}

func runRemux(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("remux", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var output string
	flags.StringVar(&output, "o", "", "output media path")
	flags.StringVar(&output, "output", "", "output media path")
	flags.Usage = func() { fmt.Fprintln(stderr, "Usage: puremux remux -o OUTPUT INPUT [INPUT ...]") }
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
	if err := media.RemuxFiles(ctx, flags.Args(), output, media.MuxOptions{}); err != nil {
		fmt.Fprintf(stderr, "puremux: remux: %v\n", err)
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
	fmt.Fprintln(w, "  puremux remux -o OUTPUT INPUT [INPUT ...]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Output may be MP4 (.mp4/.m4a/.m4v), WebM (.webm), Matroska (.mkv/.mka), or MPEG-TS (.ts/.m2ts).")
}
