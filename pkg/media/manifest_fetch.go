package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/famomatic/puremux/internal/manifest"
)

func manifestOperationContext(ctx, root context.Context) (context.Context, func()) {
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(root, cancel)
	return operation, func() { stop(); cancel() }
}

// Preserve the final retrieval URI: relative manifest references resolve
// against the resource actually retrieved, including redirects (RFC 3986).
func fetchManifestResource(ctx, root context.Context, client *http.Client, header http.Header, rawURL string, byteRange manifest.ByteRange, limit int64) ([]byte, string, error) {
	operation, cleanup := manifestOperationContext(ctx, root)
	defer cleanup()
	if limit <= 0 || limit == math.MaxInt64 {
		return nil, "", ErrInvalidData
	}
	req, err := http.NewRequestWithContext(operation, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = header.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if byteRange.Valid {
		if byteRange.Offset < 0 || byteRange.Length <= 0 || byteRange.Offset > math.MaxInt64-byteRange.Length {
			return nil, "", ErrInvalidData
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteRange.Offset, byteRange.Offset+byteRange.Length-1))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if byteRange.Valid {
		if resp.StatusCode != http.StatusPartialContent || !validHLSContentRange(resp.Header.Get("Content-Range"), byteRange.Offset, byteRange.Offset+byteRange.Length-1) {
			return nil, "", errors.New("media: invalid manifest resource range")
		}
		limit = min(limit, byteRange.Length)
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("media: HTTP resource returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit || (byteRange.Valid && int64(len(data)) != byteRange.Length) {
		return nil, "", errors.New("media: resource size outside configured bounds")
	}
	return data, resp.Request.URL.String(), nil
}
