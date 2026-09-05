package media

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type failingOpenSource struct {
	*bytes.Reader
	cause error
}

func (s *failingOpenSource) Name() string               { return "failing" }
func (s *failingOpenSource) Close() error               { return nil }
func (s *failingOpenSource) Read(p []byte) (int, error) { return 0, s.cause }
func TestOpenPreservesInitializationError(t *testing.T) {
	for _, format := range []Format{FormatUnknown, FormatWebM, FormatOgg, FormatMP4} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("transport failed")} {
			src := &failingOpenSource{Reader: bytes.NewReader(make([]byte, 128)), cause: cause}
			_, err := Open(context.Background(), src, OpenOptions{FormatHint: format})
			if !errors.Is(err, cause) || !errors.Is(err, ErrInvalidData) {
				t.Errorf("format %v: %v lost %v", format, err, cause)
			}
		}
	}
}
