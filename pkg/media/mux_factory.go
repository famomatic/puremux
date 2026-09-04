package media

import (
	"fmt"
	"io"
)

// NewMuxer creates a compressed-packet muxer for the explicitly selected
// output format. The destination remains owned by the caller and is never
// closed by the muxer.
func NewMuxer(w io.Writer, opts MuxOptions) (Muxer, error) {
	if err := validateMuxOptions(w, opts); err != nil {
		return nil, err
	}
	switch opts.Format {
	case FormatMP4:
		return newMP4Muxer(w, opts)
	case FormatWebM, FormatMatroska:
		return newEBMLMuxer(w, opts.Format)
	case FormatMPEGTS:
		return newMPEGTSMuxer(w), nil
	default:
		return nil, fmt.Errorf("%w: %s output", ErrUnsupportedFormat, opts.Format)
	}
}
