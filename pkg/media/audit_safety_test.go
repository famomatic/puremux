package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/famomatic/puremux/internal/manifest"
)

func TestRemuxRollbackFailureReportsBackup(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	temporary := filepath.Join(dir, "temporary")
	if err := os.WriteFile(output, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	installErr := errors.New("install failure")
	restoreErr := errors.New("restore failure")
	rename := func(from, to string) error {
		if from == temporary {
			return installErr
		}
		if to == output {
			return restoreErr
		}
		return os.Rename(from, to)
	}
	err := installRemuxOutputWithRename(temporary, output, rename)
	if !errors.Is(err, installErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("lost error: %v", err)
	}
	backups, e := filepath.Glob(filepath.Join(dir, ".puremux-backup-*"))
	if e != nil || len(backups) != 1 {
		t.Fatalf("backups %v: %v", backups, e)
	}
	data, e := os.ReadFile(backups[0])
	if e != nil || string(data) != "original" || !strings.Contains(err.Error(), backups[0]) {
		t.Fatalf("backup lost: %q %v", data, err)
	}
}

func TestManifestEndWithoutNewSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:4\n#EXTINF:1,\ns.mp4\n#EXT-X-ENDLIST\n")
	}))
	defer server.Close()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &hlsDemuxer{client: server.Client(), playlistURL: server.URL, root: root, opts: HLSOptions{MaxManifestBytes: 4096, MaxEntries: 10}, currentSeg: manifest.HLSSegment{Sequence: 4}}
	if err := d.refresh(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !d.playlist.EndList {
		t.Fatal("end state lost")
	}
}

func TestMP4LanguageAndUnsupportedMetadata(t *testing.T) {
	stream := auditAACStream()
	stream.Language = "kor"
	var output bytes.Buffer
	m, err := NewMuxer(&output, MuxOptions{Format: FormatMP4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.AddStream(stream); err != nil {
		t.Fatal(err)
	}
	packet := &Packet{PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1024), Data: []byte{1}}
	packet.DiscardPadding = time.Millisecond
	if err = m.WritePacket(context.Background(), packet); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatal(err)
	}
	packet.DiscardPadding = 0
	if err = m.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(context.Background(), MemorySource("language.mp4", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Streams()[0].Language != "kor" {
		t.Fatal(d.Streams())
	}
	// ISO-639 packed 5-bit MSB-first: k=11, o=15, r=18 -> 0x2df2.
	if !bytes.Contains(output.Bytes(), []byte{0x2d, 0xf2, 0, 0}) {
		t.Fatal("missing packed language")
	}
	stream.Metadata = map[string]string{"title": "must not disappear"}
	m, _ = NewMuxer(io.Discard, MuxOptions{Format: FormatMP4})
	if _, err = m.AddStream(stream); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatal(err)
	}
	m, _ = NewMuxer(io.Discard, MuxOptions{Format: FormatMP4, AllowMetadataLoss: true})
	if _, err = m.AddStream(stream); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
}

func TestEBMLPreservesTrackMetadata(t *testing.T) {
	var output bytes.Buffer
	m, err := NewMuxer(&output, MuxOptions{Format: FormatWebM})
	if err != nil {
		t.Fatal(err)
	}
	stream := Stream{Codec: CodecVP8, Type: MediaVideo, TimeBase: Rational{Num: 1, Den: 1000}, Width: 16, Height: 16, Language: "kor", Disposition: DispositionDefault, Metadata: map[string]string{"title": "Camera"}}
	if _, err = m.AddStream(stream); err != nil {
		t.Fatal(err)
	}
	if err = m.WritePacket(context.Background(), &Packet{PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(40), Flags: PacketKeyframe, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(context.Background(), MemorySource("metadata.webm", output.Bytes()), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := d.Streams()[0]
	if got.Language != "kor" || got.Metadata["title"] != "Camera" || got.Disposition != DispositionDefault {
		t.Fatalf("%+v", got)
	}
}
