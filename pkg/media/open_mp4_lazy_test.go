package media

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestIntegrationAuditFragmentedMP4StartsBeforeTail(t *testing.T) {
	var b bytes.Buffer
	m, err := NewMuxer(&b, MuxOptions{Format: FormatMP4, MP4Mode: MP4ModeFragmented, FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.AddStream(auditAACStream())
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2; i++ {
		if err = m.WritePacket(context.Background(), &Packet{PTS: KnownTimestamp(i * 1024), DTS: KnownTimestamp(i * 1024), Duration: KnownTimestamp(1024), Data: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	first := bytes.Index(data, []byte("moof"))
	second := bytes.Index(data[first+4:], []byte("moof")) + first + 4 - 4
	s := &auditDownloadPrefix{Reader: bytes.NewReader(data), available: int64(second)}
	d, err := Open(context.Background(), s, OpenOptions{})
	if err != nil {
		t.Fatalf("complete init + first fragment available: %v", err)
	}
	defer d.Close()
	p, err := d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, err = d.Seek(context.Background(), SeekRequest{StreamIndex: 0, Target: 0}); err == nil {
		t.Fatal("index unexpectedly available")
	}
	s.available = int64(len(data))
	p, err = d.ReadPacket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PTS.Value != 1024 {
		t.Fatalf("second fragment PTS: %+v", p.PTS)
	}
	p.Release()

}
