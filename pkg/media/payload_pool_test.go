package media

import (
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

func TestPooledPayloadExclusiveAndIdempotentRelease(t *testing.T) {
	a := &Packet{Data: core.AcquirePayload(16), release: core.ReleasePayload}
	b := &Packet{Data: core.AcquirePayload(16), release: core.ReleasePayload}
	a.Data[0] = 1
	b.Data[0] = 2
	if a.Data[0] != 1 {
		t.Fatal("aliased active buffers")
	}
	a.Release()
	a.Release()
	if b.Data[0] != 2 {
		t.Fatal("release changed active buffer")
	}
	b.Release()
}
