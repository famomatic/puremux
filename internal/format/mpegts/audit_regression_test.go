package mpegts

import (
	"github.com/famomatic/puremux/internal/core"
	"testing"
)

func TestStreamingPendingPacketAndByteBounds(t *testing.T) {
	for _, byteLimit := range []bool{false, true} {
		s := &StreamingInputReader{trackByPID: map[uint16]int{256: 0}, tracks: []InputTrack{{PID: 256, Codec: core.CodecH264, Timescale: 90000}}, clocks: make(map[uint16]*pesClock)}
		if byteLimit {
			s.pendingBytes = 64 << 20
		} else {
			s.pending = make([]InputPacket, 4096)
		}
		// MPEG PES PTS marker packing uses the existing spec-derived writer suite;
		// 0x65 is MSB-first forbidden=0, nal_ref_idc=3, nal_unit_type=5 (IDR).
		data := append(pesHeader(0xe0, 0, 0, 5), 0, 0, 1, 0x65, 0)
		if err := s.finishPES(&pesBuffer{pid: 256, data: data}); err == nil {
			t.Fatalf("limit bypass: bytes=%v", byteLimit)
		}
	}
}

func TestTSUsesValidatedHEVCKeyframeDetector(t *testing.T) {
	// H.265 two-byte NAL header, MSB-first: type=20 -> 0x28;
	// temporal_id_plus1 is low three bits of byte 1 and must not be zero.
	for _, tc := range []struct {
		data []byte
		want bool
	}{
		{nil, false}, {[]byte{0, 0, 1, 0x28}, false},
		{[]byte{0, 0, 1, 0x28, 0}, false}, {[]byte{0, 0, 1, 0xa8, 1}, false},
		{[]byte{0, 0, 1, 0x28, 1, 0xaa}, true},
	} {
		if got := annexBKeyframe(core.CodecHEVC, tc.data); got != tc.want {
			t.Fatalf("%x: %v", tc.data, got)
		}
	}
}
