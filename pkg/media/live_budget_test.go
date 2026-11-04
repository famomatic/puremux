package media

import (
	"testing"

	"github.com/famomatic/puremux/internal/core"
)

func TestLiveRetainedByteBudget(t *testing.T) {
	l := &LiveMuxer{maxBufferBytes: 10}
	a := l.copyPacket(make([]byte, 6))
	if a == nil {
		t.Fatal("first reservation")
	}
	if p := l.copyPacket(make([]byte, 5)); p != nil {
		core.ReleasePacket(p)
		t.Fatal("budget exceeded")
	}
	core.ReleasePacket(a)
	if l.retainedBytes != 0 {
		t.Fatal(l.retainedBytes)
	}
	b := l.copyPacket(make([]byte, 10))
	if b == nil {
		t.Fatal("budget not released")
	}
	core.ReleasePacket(b)
}
