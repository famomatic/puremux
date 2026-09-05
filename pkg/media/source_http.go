package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// HTTPRetryPolicy decides whether an HTTP range request should be retried.
// attempt is zero for the first failure. status is zero for transport errors.
type HTTPRetryPolicy func(attempt, status int, err error) bool

type HTTPSourceOptions struct {
	Client      *http.Client
	Header      http.Header
	MaxRetries  int
	RetryPolicy HTTPRetryPolicy
}

// HTTPSource is a bounded, seekable HTTP byte source. Every data request is
// an explicit Range request and every Content-Range response is validated.
type HTTPSource struct {
	client     *http.Client
	url        string
	header     http.Header
	size       int64
	etag       string
	modified   string
	maxRetries int
	retry      HTTPRetryPolicy

	stateMu sync.Mutex
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc

	posMu           sync.Mutex
	pos             int64
	readAhead       []byte
	readAheadOffset int64
}

// OpenHTTP verifies byte-range support with a one-byte request. client is
// caller-owned and is never closed. The returned source owns no response body.
func OpenHTTP(ctx context.Context, rawURL string, opts HTTPSourceOptions) (*HTTPSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rawURL == "" {
		return nil, errors.New("media: empty HTTP URL")
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	root, cancel := context.WithCancel(context.Background())
	s := &HTTPSource{
		client:     client,
		url:        rawURL,
		header:     opts.Header.Clone(),
		maxRetries: max(opts.MaxRetries, 0),
		retry:      opts.RetryPolicy,
		ctx:        root,
		cancel:     cancel,
	}
	if s.header == nil {
		s.header = make(http.Header)
	}
	size, etag, modified, err := s.probe(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	s.size, s.etag, s.modified = size, etag, modified
	return s, nil
}

func (s *HTTPSource) Name() string { return s.url }
func (s *HTTPSource) Size() int64  { return s.size }

func (s *HTTPSource) Close() error {
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	s.stateMu.Unlock()
	return nil
}

func (s *HTTPSource) Read(p []byte) (int, error) {
	return s.ReadContext(context.Background(), p)
}

func (s *HTTPSource) ReadContext(ctx context.Context, p []byte) (int, error) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.stateMu.Lock()
	closed := s.closed
	s.stateMu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	const readAheadSize = 32 << 10
	if len(p) >= readAheadSize || s.pos >= s.size {
		n, err := s.ReadAtContext(ctx, p, s.pos)
		s.pos += int64(n)
		return n, err
	}
	if s.pos < s.readAheadOffset || s.pos >= s.readAheadOffset+int64(len(s.readAhead)) {
		size := min(int64(readAheadSize), s.size-s.pos)
		if cap(s.readAhead) < int(size) {
			s.readAhead = make([]byte, size)
		} else {
			s.readAhead = s.readAhead[:size]
		}
		n, err := s.ReadAtContext(ctx, s.readAhead, s.pos)
		if err != nil {
			s.readAhead = nil
			return 0, err
		}
		s.readAhead = s.readAhead[:n]
		s.readAheadOffset = s.pos
	}
	n := copy(p, s.readAhead[s.pos-s.readAheadOffset:])
	s.pos += int64(n)
	if n < len(p) && s.pos == s.size {
		return n, io.EOF
	}
	return n, nil
}

func (s *HTTPSource) ReadAt(p []byte, off int64) (int, error) {
	return s.ReadAtContext(context.Background(), p, off)
}

func (s *HTTPSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.stateMu.Lock()
	closed := s.closed
	s.stateMu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("media: negative HTTP read offset")
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	short := false
	if remaining := s.size - off; want > remaining {
		want = remaining
		short = true
	}
	requestCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	n, err := s.fetch(requestCtx, p[:want], off, off+want-1)
	if err != nil {
		return n, err
	}
	if short {
		return n, io.EOF
	}
	return n, nil
}

func (s *HTTPSource) Seek(offset int64, whence int) (int64, error) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	s.stateMu.Lock()
	closed := s.closed
	s.stateMu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = s.pos
	case io.SeekEnd:
		base = s.size
	default:
		return 0, errors.New("media: invalid HTTP seek whence")
	}
	if (offset > 0 && base > int64(^uint64(0)>>1)-offset) || (offset < 0 && base < -int64(^uint64(0)>>1)-1-offset) {
		return 0, errors.New("media: HTTP seek overflow")
	}
	next := base + offset
	if next < 0 {
		return 0, errors.New("media: negative HTTP seek position")
	}
	// Explicit seeks revalidate the resource on the next read.
	s.readAhead = s.readAhead[:0]
	s.pos = next
	return next, nil
}

func (s *HTTPSource) operationContext(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil, nil, ErrClosed
	}
	root := s.ctx
	s.stateMu.Unlock()
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(root, cancel)
	return requestCtx, func() {
		stop()
		cancel()
	}, nil
}

func (s *HTTPSource) probe(ctx context.Context) (int64, string, string, error) {
	resp, err := s.do(ctx, 0, 0, false)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		total, ok := parseUnsatisfiedRange(resp.Header.Get("Content-Range"))
		if ok && total == 0 {
			return 0, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), nil
		}
		return 0, "", "", fmt.Errorf("media: HTTP range probe: %s", resp.Status)
	}
	if resp.StatusCode == http.StatusOK {
		return 0, "", "", fmt.Errorf("%w: server ignored Range", ErrNotSeekable)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, "", "", fmt.Errorf("media: HTTP range probe: %s", resp.Status)
	}
	start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != 0 || end != 0 || total <= 0 {
		return 0, "", "", errors.New("media: invalid HTTP Content-Range probe")
	}
	var one [1]byte
	if _, err := io.ReadFull(resp.Body, one[:]); err != nil {
		return 0, "", "", fmt.Errorf("media: truncated HTTP range probe: %w", err)
	}
	return total, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), nil
}

func (s *HTTPSource) fetch(ctx context.Context, dst []byte, start, end int64) (int, error) {
	resp, err := s.do(ctx, start, end, true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0, ErrSourceChanged
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("media: HTTP range read: %s", resp.Status)
	}
	if (s.etag != "" && resp.Header.Get("ETag") != s.etag) ||
		(s.etag == "" && s.modified != "" && resp.Header.Get("Last-Modified") != s.modified) {
		return 0, ErrSourceChanged
	}
	gotStart, gotEnd, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || gotStart != start || gotEnd != end || total != s.size {
		return 0, errors.New("media: invalid HTTP Content-Range response")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, errors.New("media: ranged response used content encoding")
	}
	n, err := io.ReadFull(resp.Body, dst)
	if err != nil {
		return n, fmt.Errorf("media: truncated HTTP range response: %w", err)
	}
	var extra [1]byte
	if extraN, extraErr := resp.Body.Read(extra[:]); extraN != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
		return n, errors.New("media: oversized HTTP range response")
	}
	return n, nil
}

func (s *HTTPSource) do(ctx context.Context, start, end int64, conditional bool) (*http.Response, error) {
	var lastStatus int
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
		if err != nil {
			return nil, err
		}
		req.Header = s.header.Clone()
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		if conditional {
			if s.etag != "" {
				req.Header.Set("If-Range", s.etag)
			} else if s.modified != "" {
				req.Header.Set("If-Range", s.modified)
			}
		}
		resp, requestErr := s.client.Do(req)
		if requestErr == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil
		}
		lastErr = requestErr
		lastStatus = 0
		if resp != nil {
			lastStatus = resp.StatusCode
			_ = resp.Body.Close()
		}
		if attempt >= s.maxRetries || s.retry == nil || !s.retry(attempt, lastStatus, lastErr) {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("media: HTTP request failed with status %d", lastStatus)
		}
	}
}

func parseContentRange(value string) (start, end, total int64, ok bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 || parts[1] == "*" {
		return 0, 0, 0, false
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}
	start, errStart := strconv.ParseInt(bounds[0], 10, 64)
	end, errEnd := strconv.ParseInt(bounds[1], 10, 64)
	total, errTotal := strconv.ParseInt(parts[1], 10, 64)
	return start, end, total, errStart == nil && errEnd == nil && errTotal == nil && start >= 0 && end >= start && total > end
}

func parseUnsatisfiedRange(value string) (int64, bool) {
	if !strings.HasPrefix(value, "bytes */") {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(value, "bytes */"), 10, 64)
	return total, err == nil && total >= 0
}
