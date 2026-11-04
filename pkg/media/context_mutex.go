package media

import (
	"context"
	"sync"
)

// contextMutex admits one operation and permits queued operations to cancel.
// Its zero value is ready for use; it must not be copied after first use.
type contextMutex struct {
	once sync.Once
	gate chan struct{}
}

func (m *contextMutex) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.once.Do(func() { m.gate = make(chan struct{}, 1) })
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-m.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextMutex) Lock()   { _ = m.lock(context.Background()) }
func (m *contextMutex) Unlock() { <-m.gate }
