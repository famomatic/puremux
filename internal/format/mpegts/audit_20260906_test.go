package mpegts

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
)

func audit20260906TS(t *testing.T) []byte {
	var out bytes.Buffer
	w := New(&out)
	id, err := w.AddTrack(core.Track{Kind: core.TrackAudio, Codec: core.CodecAAC})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := aac.WrapADTS(aac.Config{AudioObjectType: 2, FrequencyIndex: 3, SampleRate: 48000, ChannelConfig: 2}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.WritePacket(&core.Packet{TrackID: id, Codec: core.CodecAAC, Data: frame}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestAudit20260906TSNullPackets(t *testing.T) {
	// H.222.0 2.4.3.3: PID=0x1fff, payload only 01, CC undefined (both 0).
	null := bytes.Repeat([]byte{0xff}, 188)
	copy(null, []byte{0x47, 0x1f, 0xff, 0x10})
	data := append(append(append([]byte{}, null...), null...), audit20260906TS(t)...)
	if _, err := NewInputReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid null packets rejected: %v", err)
	}
}
func TestAudit20260906TSDuplicatePacket(t *testing.T) {
	data := audit20260906TS(t)
	dup := append(append([]byte{}, data[:188]...), data...)
	if _, err := NewInputReader(bytes.NewReader(dup)); err != nil {
		t.Fatalf("allowed byte-identical duplicate PAT rejected: %v", err)
	}
}
func TestAudit20260906TSSplitPSI(t *testing.T) {
	// H.222.0 PAT: 5 bytes fixed after length + 50*4 program entries + CRC4=209 (0xd1).
	section := []byte{0, 0xb0, 0xd1, 0, 1, 0xc1, 0, 0}
	for i := 0; i < 50; i++ {
		section = append(section, 0, byte(i+1), 0xe0|byte((0x1000+i)>>8), byte(0x1000+i))
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32MPEG(section))
	section = append(section, crc[:]...)
	if crc32MPEG(section) != 0 {
		t.Fatal("bad fixture CRC")
	}
	first := bytes.Repeat([]byte{0xff}, 188)
	copy(first, []byte{0x47, 0x40, 0, 0x10, 0})
	copy(first[5:], section[:183])
	second := bytes.Repeat([]byte{0xff}, 188)
	copy(second, []byte{0x47, 0, 0, 0x11})
	copy(second[4:], section[183:])
	original := audit20260906TS(t)
	data := append(append(first, second...), original[188:]...)
	if _, err := NewInputReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid multi-packet PAT rejected: %v", err)
	}
}
func TestAudit20260906PTSWrapReorder(t *testing.T) {
	var epoch timestampEpoch
	mod := int64(1) << 33
	// Decode-order PTS can cross the modulo boundary back and forth due to B-frames.
	raw := []int64{mod - 2, 2, mod - 1, 3}
	want := []int64{mod - 2, mod + 2, mod - 1, mod + 3}
	for i, v := range raw {
		got, err := epoch.unwrap(v)
		if err != nil || got != want[i] {
			t.Fatalf("PTS[%d]=%d want %d err=%v", i, got, want[i], err)
		}
	}
}
func TestAudit20260906TSInPESConfigChange(t *testing.T) {
	// Both headers valid in isolation: LC48kHz/stereo then LC44.1kHz/stereo.
	// Header byte2 4c -> 50 changes only sampling_frequency_index from3 to4.
	payload := []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x1f, 0xfc, 1, 0xff, 0xf1, 0x50, 0x80, 1, 0x1f, 0xfc, 2}
	data := append(pesHeader(0xc0, 900000, 900000, len(payload)), payload...)
	packets, track, err := parsePES(&pesBuffer{pid: 256, data: data}, 0, core.CodecAAC)
	if err == nil {
		t.Fatalf("in-PES rate switch accepted as %dHz; second PTS=%d", track.SampleRate, packets[1].PTS)
	}
}
