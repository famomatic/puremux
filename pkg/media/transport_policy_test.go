package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPPositionQueryRetainsReadAhead(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		a, b, _ := testRange(r.Header.Get("Range"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/100", a, b))
		w.WriteHeader(206)
		w.Write(make([]byte, b-a+1))
	}))
	defer srv.Close()
	s, err := OpenHTTP(context.Background(), srv.URL, HTTPSourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 10 {
		if _, err = s.Seek(0, io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("requests=%d, want probe+one read", calls.Load())
	}
}
func TestManifestHeaderPolicy(t *testing.T) {
	fallback := http.Header{"Authorization": []string{"secret"}}
	policy := func(u string) http.Header {
		if u == "https://trusted.test/segment" {
			return fallback
		}
		return nil
	}
	if got := manifestHeaders(fallback, policy, "https://elsewhere.test/key").Get("Authorization"); got != "" {
		t.Fatal("cross-origin credential")
	}
	got := manifestHeaders(fallback, policy, "https://trusted.test/segment")
	got.Set("Authorization", "changed")
	if fallback.Get("Authorization") != "secret" {
		t.Fatal("header aliases policy storage")
	}
}
