package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPSourceRangeReadSeekAndValidators(t *testing.T) {
	data := []byte("0123456789")
	var sawHeader atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") == "yes" {
			sawHeader.Store(true)
		}
		ifRange := r.Header.Get("If-Range")
		if ifRange != "" && ifRange != `"v1"` {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		start, end, ok := testRange(r.Header.Get("Range"))
		if !ok || start < 0 || end >= int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	source, err := OpenHTTP(context.Background(), server.URL, HTTPSourceOptions{
		Client: server.Client(),
		Header: http.Header{"X-Test": []string{"yes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.Size() != int64(len(data)) || !sawHeader.Load() {
		t.Fatalf("size/header = %d/%v", source.Size(), sawHeader.Load())
	}
	p := make([]byte, 4)
	if n, err := source.ReadAt(p, 3); err != nil || n != 4 || string(p) != "3456" {
		t.Fatalf("ReadAt = %d, %v, %q", n, err, p)
	}
	if _, err := source.Seek(-2, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	p = make([]byte, 4)
	if n, err := source.Read(p); !errors.Is(err, io.EOF) || n != 2 || string(p[:n]) != "89" {
		t.Fatalf("short Read = %d, %v, %q", n, err, p[:n])
	}
	if _, err := source.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative ReadAt succeeded")
	}
}

func TestHTTPSourceRejectsInvalidRangeServers(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{
			name: "ignored",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("all"))
			},
			want: ErrNotSeekable,
		},
		{
			name: "wrong content range",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", "bytes 1-1/10")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte{0})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			_, err := OpenHTTP(context.Background(), server.URL, HTTPSourceOptions{Client: server.Client()})
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("OpenHTTP = %v, want %v", err, test.want)
			}
		})
	}
}

func TestHTTPSourceRetryCancellationChangeAndClose(t *testing.T) {
	data := []byte("abc")
	var calls atomic.Int32
	var changed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if changed.Load() && r.Header.Get("If-Range") != "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		start, end, _ := testRange(r.Header.Get("Range"))
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()
	source, err := OpenHTTP(context.Background(), server.URL, HTTPSourceOptions{
		Client:     server.Client(),
		MaxRetries: 1,
		RetryPolicy: func(attempt, status int, err error) bool {
			return attempt == 0 && status == http.StatusServiceUnavailable && err == nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	if _, err := source.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed source = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if _, err := source.ReadAt(nil, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("zero-length read after close = %v", err)
	}

	blocked := make(chan struct{})
	cancelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/2")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
			return
		}
		close(blocked)
		<-r.Context().Done()
	}))
	defer cancelServer.Close()
	cancelSource, err := OpenHTTP(context.Background(), cancelServer.URL, HTTPSourceOptions{Client: cancelServer.Client()})
	if err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(context.Background())
	zeroCtx, cancelZero := context.WithCancel(context.Background())
	cancelZero()
	if _, err := cancelSource.ReadAtContext(zeroCtx, nil, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("zero-length canceled read = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cancelSource.ReadAtContext(readCtx, make([]byte, 1), 1)
		done <- err
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("range request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled read did not unblock")
	}
	_ = cancelSource.Close()
}

func TestHTTPSourceRejectsMissingValidatorAfterProbe(t *testing.T) {
	data := []byte("ab")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := testRange(r.Header.Get("Range"))
		if !ok {
			t.Errorf("missing or invalid Range: %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"v1"`)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	source, err := OpenHTTP(context.Background(), server.URL, HTTPSourceOptions{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.ReadAt(make([]byte, 1), 1); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("read without probed ETag = %v, want ErrSourceChanged", err)
	}
}

func testRange(value string) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, errStart := strconv.ParseInt(parts[0], 10, 64)
	end, errEnd := strconv.ParseInt(parts[1], 10, 64)
	return start, end, errStart == nil && errEnd == nil
}
