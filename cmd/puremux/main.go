// Command puremux reads raw packet input from stdin and writes a WebM stream
// to stdout. This is a minimal CLI wrapper around the puremux facade.
package main

import (
	"fmt"
	"os"
)

func main() {
	// The CLI is a thin shell; the heavy lifting lives in the library. For
	// now it prints usage so the binary is non-trivial and buildable.
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Println("puremux: pure Go WebM muxer")
		fmt.Println()
		fmt.Println("Usage: puremux [-h|--help]")
		fmt.Println()
		fmt.Println("Reads raw packets from stdin and writes a WebM stream to stdout.")
		fmt.Println("This is a minimal entrypoint; see pkg/puremux for the library API.")
		return
	}
	fmt.Fprintln(os.Stderr, "puremux: no input handler wired (use the library API)")
	os.Exit(1)
}