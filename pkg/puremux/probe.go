package puremux

import (
	"fmt"
	"os"

	"github.com/famomatic/puremux/internal/core"
)

// TrackInfo describes one track detected by Probe. It carries the codec and
// kind so callers can decide compatibility without opening a muxing session.
type TrackInfo struct {
	Codec  core.CodecType
	Kind   core.TrackKind
	Number int
}

// FormatInfo is the detected container + track layout of an input file. It is
// the information surface callers need to answer "what codecs does this file
// carry?" without running a full remux.
type FormatInfo struct {
	Container Container
	Tracks    []TrackInfo
}

// Probe inspects an input file and returns its container plus the codec of
// each track. It opens the file, reads the container header, and walks the
// track table only far enough to identify codecs; it does NOT decode or hold
// any media payload (opacity invariant, ARCHITECTURE.md section 4).
//
// Probe surfaces the codecs directly: a caller that cannot tell what codecs a
// file carries uses Probe, then routes to the right muxer or transcoding step
// without a failed parse. It is the complement to the container gates - the
// gates say "can puremux handle this container pair"; Probe says "what is
// actually in this file".
func Probe(path string) (FormatInfo, error) {
	c, err := DetectContainer(path)
	if err != nil {
		return FormatInfo{}, err
	}
	// openInputReader opens the file and builds the demuxer, parsing only the
	// header/track tables. We read Tracks() then close without consuming any
	// media samples, so memory is bounded by the track table size.
	rd, err := openInputReader(path, c)
	if err != nil {
		return FormatInfo{}, err
	}
	defer rd.Close()
	tracks := rd.Tracks()
	info := FormatInfo{Container: c, Tracks: make([]TrackInfo, 0, len(tracks))}
	for _, t := range tracks {
		kind := core.TrackAudio
		if t.IsVideo {
			kind = core.TrackVideo
		}
		info.Tracks = append(info.Tracks, TrackInfo{Codec: t.Codec, Kind: kind, Number: t.Number})
	}
	return info, nil
}

// ProbeOutputContainer returns the best puremux-writable output container for
// the given input's codecs, or ContainerUnknown if none can hold all of them.
// It is a convenience over Probe + CanRemuxCodecs: WebM is preferred when all
// codecs fit (smaller, WebM-restricted); otherwise MKV is tried (superset).
func ProbeOutputContainer(path string) (Container, error) {
	info, err := Probe(path)
	if err != nil {
		return ContainerUnknown, err
	}
	codecs := make([]core.CodecType, 0, len(info.Tracks))
	for _, t := range info.Tracks {
		codecs = append(codecs, t.Codec)
	}
	if CanRemuxCodecs(ContainerWebM, codecs) {
		return ContainerWebM, nil
	}
	if CanRemuxCodecs(ContainerMKV, codecs) {
		return ContainerMKV, nil
	}
	return ContainerUnknown, fmt.Errorf("%w: no output container for codecs %v", ErrIncompatible, codecs)
}

// ensure os is referenced (Probe uses os via openInputReader which opens).
var _ = os.Open
