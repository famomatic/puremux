package media

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestValidateMuxOptions(t *testing.T) {
	tests := []struct {
		name string
		opts MuxOptions
		want error
	}{
		{name: "unknown format", opts: MuxOptions{}, want: ErrUnsupportedFormat},
		{name: "bad mode", opts: MuxOptions{Format: FormatMP4, MP4Mode: 3}, want: ErrInvalidData},
		{name: "negative duration", opts: MuxOptions{Format: FormatMP4, FragmentDuration: -time.Nanosecond}, want: ErrInvalidData},
		{name: "foreign mp4 mode", opts: MuxOptions{Format: FormatWebM, MP4Mode: MP4ModeFragmented}, want: ErrInvalidData},
		{name: "progressive non seeker", opts: MuxOptions{Format: FormatMP4, MP4Mode: MP4ModeProgressive}, want: ErrNotSeekable},
		{name: "fragmented writer", opts: MuxOptions{Format: FormatMP4, MP4Mode: MP4ModeFragmented}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateMuxOptions(&bytes.Buffer{}, test.opts)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	if err := validateMuxOptions(nil, MuxOptions{Format: FormatMP4}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("nil writer error = %v", err)
	}
}
