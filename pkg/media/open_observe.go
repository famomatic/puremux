package media

import (
	"errors"
	"io"
)

var ErrInitLimit = errors.New("media: initialization byte limit reached")

type initializationState struct {
	stats  OpenStats
	limit  int64
	active bool
}
type initializationReader struct {
	reader io.Reader
	state  *initializationState
}

func (r *initializationReader) Read(p []byte) (int, error) {
	if !r.state.active {
		return r.reader.Read(p)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.state.limit > 0 {
		left := r.state.limit - r.state.stats.BytesRead
		if left <= 0 {
			return 0, ErrInitLimit
		}
		if int64(len(p)) > left {
			p = p[:left]
		}
	}
	r.state.stats.ReadCalls++
	n, err := r.reader.Read(p)
	r.state.stats.BytesRead += int64(n)
	return n, err
}

type initializationReadSeeker struct {
	*initializationReader
	seeker io.Seeker
}

func (r *initializationReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if r.state.active {
		r.state.stats.SeekCalls++
	}
	return r.seeker.Seek(offset, whence)
}
