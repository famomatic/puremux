package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPStreamUnknownLengthAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "ok" {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte("head"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	options := HTTPSourceOptions{Client: server.Client(), Header: http.Header{"X-Test": []string{"ok"}}}
	if _, err := OpenHTTP(context.Background(), server.URL, options); !errors.Is(err, ErrNotSeekable) {
		t.Fatalf("range probe: %v", err)
	}
	src, err := OpenHTTPStream(context.Background(), server.URL, options)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var head [4]byte
	if _, err = io.ReadFull(src, head[:]); err != nil || string(head[:]) != "head" {
		t.Fatalf("prefix: %s %v", head, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = src.ReadContext(ctx, head[:]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read cancellation: %v", err)
	}
	if err = src.Close(); err != nil {
		t.Fatal(err)
	}
}
