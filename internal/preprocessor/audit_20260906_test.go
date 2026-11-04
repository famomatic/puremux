package preprocessor

import (
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

func TestAudit20260906ReorderNonPictureBound(t *testing.T) {
	s := NewPresentationSynthesizer(DefaultConfig())
	defer s.Flush(core.ReleasePacket)
	for _, au := range ipbbPOCStream(3) {
		p := core.AcquirePacket()
		p.DTS = time.Duration(au.tMs) * time.Millisecond
		p.PTS = p.DTS
		s.Process(p, core.POCInfo{Order: au.order, Epoch: 1}, true, core.ReleasePacket)
	}
	if !s.reorder {
		t.Fatal("fixture did not enter reorder mode")
	}
	for i := 0; i < 1000; i++ {
		p := core.AcquirePacket()
		p.Data = append(p.Data[:0], 1)
		s.Process(p, core.POCInfo{}, false, core.ReleasePacket)
	}
	if len(s.win) > ptsProbeMaxHold {
		t.Fatalf("reorder queue holds %d packets after 1000 non-picture AUs; public max buffers do not apply", len(s.win))
	}
}
