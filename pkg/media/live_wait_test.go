package media

import (
	"errors"
	"testing"
	"time"
)

func TestLiveWaitPreservesSentinelAndDelay(t *testing.T) {
	err := error(&LiveWaitError{Delay: 3 * time.Second})
	var retry interface{ RetryAfter() time.Duration }
	if !errors.Is(err, ErrNoNewSegments) || !errors.As(err, &retry) || retry.RetryAfter() != 3*time.Second {
		t.Fatal(err)
	}
}
