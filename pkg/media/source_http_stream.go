package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HTTPStreamSource reads a single HTTP response without requiring ranges or a
// Content-Length. Canceling an in-flight read ends the response; callers must
// reopen the source to resume. Close interrupts pending network reads.
type HTTPStreamSource struct {
	name     string
	body     io.ReadCloser
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
}

// OpenHTTPStream opens a sequential HTTP GET. Client and Header are honored;
// range read-ahead/retry options do not apply to this single response.
func OpenHTTPStream(ctx context.Context, rawURL string, opts HTTPSourceOptions) (*HTTPStreamSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	req, err := http.NewRequestWithContext(root, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = opts.Header.Clone()
	req.Header.Del("Range")
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		cancel()
		return nil, fmt.Errorf("media: HTTP stream: %s", response.Status)
	}
	if !stop() || ctx.Err() != nil {
		response.Body.Close()
		cancel()
		return nil, ctx.Err()
	}
	return &HTTPStreamSource{name: rawURL, body: response.Body, cancel: cancel}, nil
}
func (s *HTTPStreamSource) Name() string               { return s.name }
func (s *HTTPStreamSource) Read(p []byte) (int, error) { return s.ReadContext(context.Background(), p) }
func (s *HTTPStreamSource) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	n, err := s.body.Read(p)
	if ctx.Err() != nil {
		return n, ctx.Err()
	}
	return n, err
}
func (s *HTTPStreamSource) Close() error {
	s.once.Do(func() { s.cancel(); s.closeErr = s.body.Close() })
	return s.closeErr
}
