package media

import (
	"fmt"
	"io"
)

// NewMuxer creates a compressed-packet muxer for the explicitly selected
// output format. The destination remains owned by the caller and is never
// closed by the muxer.
func NewMuxer(w io.Writer, opts MuxOptions) (result Muxer, resultErr error) {
	if err := validateMuxOptions(w, opts); err != nil {
		return nil, err
	}
	state := &outputFailure{}
	writer := &failureWriter{Writer: w, state: state}
	if seeker, ok := w.(io.Seeker); ok {
		w = &failureWriteSeeker{failureWriter: writer, seeker: seeker}
	} else {
		w = writer
	}
	defer func() {
		if resultErr == nil {
			switch m := result.(type) {
			case *mp4Muxer:
				m.outputFailure = state
			case *ebmlMuxer:
				m.outputFailure = state
			case *mpegtsMuxer:
				m.outputFailure = state
			}
		}
	}()
	switch opts.Format {
	case FormatMP4:
		return newMP4Muxer(w, opts)
	case FormatWebM, FormatMatroska:
		m, err := newEBMLMuxer(w, opts.Format)
		if err == nil {
			m.(*ebmlMuxer).allowMetadataLoss = opts.AllowMetadataLoss
		}
		return m, err
	case FormatMPEGTS:
		m := newMPEGTSMuxer(w)
		m.(*mpegtsMuxer).allowMetadataLoss = opts.AllowMetadataLoss
		return m, nil
	default:
		return nil, fmt.Errorf("%w: %s output", ErrUnsupportedFormat, opts.Format)
	}
}
