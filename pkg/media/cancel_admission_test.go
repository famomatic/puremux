package media

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContextMutexCancelWhileQueued(t *testing.T) {
	var m contextMutex
	m.Lock()
	defer m.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.lock(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued lock ignored cancellation")
	}
}
