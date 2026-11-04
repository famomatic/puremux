package media

import (
	"context"
	"errors"
	"io"
	"testing"
)

type failOnceOutput struct{ calls int }

func (w *failOnceOutput) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return 1, io.ErrUnexpectedEOF
	}
	return len(p), nil
}
func TestMuxOutputFailureIsTerminal(t *testing.T) {
	for _, format := range []Format{FormatMP4, FormatMPEGTS, FormatMatroska} {
		t.Run(format.String(), func(t *testing.T) {
			out := &failOnceOutput{}
			m, err := NewMuxer(out, MuxOptions{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			s := Stream{Type: MediaAudio, Codec: CodecAAC, SampleRate: 48000, Channels: 2, TimeBase: Rational{1, 48000}, Config: CodecConfig{Format: CodecConfigASC, Data: []byte{0x11, 0x90}}}
			data := []byte{1}
			if format == FormatMPEGTS {
				data = []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x1f, 0xfc, 1}
			}
			if format == FormatMatroska {
				s.Codec = CodecOpus
				s.Config = CodecConfig{Format: CodecConfigOpusHead, Data: append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)}
				data = []byte{0xf8, 0xff, 0xfe}
			}
			id, err := m.AddStream(s)
			if err != nil {
				t.Fatal(err)
			}
			p := &Packet{StreamIndex: id, Data: data, PTS: KnownTimestamp(0), DTS: KnownTimestamp(0), Duration: KnownTimestamp(1024)}
			err = m.WritePacket(context.Background(), p)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("first=%v", err)
			}
			calls := out.calls
			if err = m.WritePacket(context.Background(), p); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
			if err = m.Close(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
			if out.calls != calls {
				t.Fatalf("wrote again after failure: %d -> %d", calls, out.calls)
			}
		})
	}
}
