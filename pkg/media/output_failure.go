package media

import "io"

// outputFailure preserves the first irreversible destination error. Validation
// errors do not poison a muxer; only actual output I/O does.
type outputFailure struct{ err error }

func (f *outputFailure) failure() error {
	if f == nil {
		return nil
	}
	return f.err
}

type failureWriter struct {
	io.Writer
	state *outputFailure
}

func (w *failureWriter) Write(p []byte) (int, error) {
	if w.state.err != nil {
		return 0, w.state.err
	}
	n, err := w.Writer.Write(p)
	if n < 0 || n > len(p) {
		n = 0
		err = io.ErrShortWrite
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.state.err = err
	}
	return n, err
}

type failureWriteSeeker struct {
	*failureWriter
	seeker io.Seeker
}

func (w *failureWriteSeeker) Seek(off int64, whence int) (int64, error) {
	if w.state.err != nil {
		return 0, w.state.err
	}
	n, err := w.seeker.Seek(off, whence)
	if err != nil {
		w.state.err = err
	}
	return n, err
}
