package media

import "time"

// LiveWaitError means that the same demuxer can be retried after Delay.
// It preserves errors.Is(err, ErrNoNewSegments) compatibility.
type LiveWaitError struct{ Delay time.Duration }

func (e *LiveWaitError) Error() string             { return ErrNoNewSegments.Error() }
func (e *LiveWaitError) Unwrap() error             { return ErrNoNewSegments }
func (e *LiveWaitError) RetryAfter() time.Duration { return e.Delay }
