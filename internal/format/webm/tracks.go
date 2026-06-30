package webm

import (
	"io"

	"github.com/famomatic/puremux/internal/core"
)

// TrackSpec is the muxer's view of a track for Tracks-element serialization.
type TrackSpec struct {
	Number       uint64 // TrackNumber (1-based)
	UID          uint64 // TrackUID
	Codec        core.CodecType
	IsVideo      bool
	Width        int
	Height       int
	Channels     int
	SampleRate   float64
	CodecPrivate []byte // optional (e.g. Opus headers, VP9 codec features)
}

// codecIDFor returns the Matroska CodecID string for a codec.
func codecIDFor(c core.CodecType) string {
	switch c {
	case core.CodecVP8:
		return codecIDVP8
	case core.CodecVP9:
		return codecIDVP9
	case core.CodecAV1:
		return codecIDAV1
	case core.CodecOpus:
		return codecIDOpus
	default:
		return ""
	}
}

// WriteTracks writes the Tracks master element with one TrackEntry per spec.
// Returns the byte offset where the Tracks element begins (for SeekHead).
func WriteTracks(w io.Writer, tracks []TrackSpec) (int64, error) {
	// Build the Tracks payload in a buffer so we can size it up front.
	var buf trackedBuf
	for _, t := range tracks {
		if err := writeTrackEntry(&buf, t); err != nil {
			return 0, err
		}
	}
	// Record start offset if w is a seeker; otherwise 0.
	start := int64(0)
	if ws, ok := w.(interface{ Seek(int64, int) (int64, error) }); ok {
		start, _ = ws.Seek(0, io.SeekCurrent)
	}
	if err := writeID(w, idTracks); err != nil {
		return 0, err
	}
	if err := writeSize(w, uint64(buf.Len())); err != nil {
		return 0, err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return start, nil
}

// writeTrackEntry serializes a single TrackEntry.
func writeTrackEntry(w io.Writer, t TrackSpec) error {
	var inner trackedBuf
	if err := writeUint(&inner, idTrackNumber, t.Number); err != nil {
		return err
	}
	if err := writeUint(&inner, idTrackUID, t.UID); err != nil {
		return err
	}
	tt := trackTypeAudio
	if t.IsVideo {
		tt = trackTypeVideo
	}
	if err := writeUint(&inner, idTrackType, tt); err != nil {
		return err
	}
	if err := writeUint(&inner, idFlagLacing, 0); err != nil {
		return err
	}
	if err := writeString(&inner, idCodecID, codecIDFor(t.Codec)); err != nil {
		return err
	}
	if len(t.CodecPrivate) > 0 {
		if err := writeBinary(&inner, idCodecPrivate, t.CodecPrivate); err != nil {
			return err
		}
	}
	if t.IsVideo {
		if err := writeVideo(&inner, t); err != nil {
			return err
		}
	} else {
		if err := writeAudio(&inner, t); err != nil {
			return err
		}
	}
	return writeElement(w, idTrackEntry, inner.Bytes())
}

func writeVideo(w io.Writer, t TrackSpec) error {
	var inner trackedBuf
	if err := writeUint(&inner, idPixelWidth, uint64(t.Width)); err != nil {
		return err
	}
	if err := writeUint(&inner, idPixelHeight, uint64(t.Height)); err != nil {
		return err
	}
	return writeElement(w, idVideo, inner.Bytes())
}

func writeAudio(w io.Writer, t TrackSpec) error {
	var inner trackedBuf
	if err := writeUint(&inner, idChannels, uint64(t.Channels)); err != nil {
		return err
	}
	if t.SampleRate > 0 {
		if err := writeFloat(&inner, idSamplingFrequency, t.SampleRate); err != nil {
			return err
		}
	}
	return writeElement(w, idAudio, inner.Bytes())
}
