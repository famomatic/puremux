package webm

import (
	"encoding/binary"
	"io"
	"math"

	"famomatic/puremux/internal/format/ebml"
)

// CuePoint is a seek index entry pointing at a Cluster.
type CuePoint struct {
	Timecode         uint64 // absolute ms
	Track            uint64 // track number
	ClusterPosition uint64 // byte offset relative to Segment body start
}

// PatchDuration overwrites the reserved Duration float at the given offset
// with the actual segment duration (in seconds, IEEE-754 double BE).
func PatchDuration(ws writeSeeker, offset int64, durationSec float64) error {
	cur, err := ws.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := ws.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(durationSec))
	if _, err := ws.Write(buf[:]); err != nil {
		return err
	}
	_, err = ws.Seek(cur, io.SeekStart)
	return err
}

// WriteCues writes the Cues master element with the given cue points.
// Returns the offset where the Cues element begins (for SeekHead).
func WriteCues(ws writeSeeker, cues []CuePoint) (int64, error) {
	start, err := ws.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	var buf trackedBuf
	for _, c := range cues {
		if err := writeCuePoint(&buf, c); err != nil {
			return 0, err
		}
	}
	if err := writeID(ws, idCues); err != nil {
		return 0, err
	}
	if err := writeSize(ws, uint64(buf.Len())); err != nil {
		return 0, err
	}
	if _, err := ws.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return start, nil
}

func writeCuePoint(w io.Writer, c CuePoint) error {
	var inner trackedBuf
	if err := writeUint(&inner, idCueTime, c.Timecode); err != nil {
		return err
	}
	// CueTrackPositions
	var pos trackedBuf
	if err := writeUint(&pos, idCueTrack, c.Track); err != nil {
		return err
	}
	if err := writeUint(&pos, idCueClusterPosition, c.ClusterPosition); err != nil {
		return err
	}
	if err := writeElement(&inner, idCueTrackPositions, pos.Bytes()); err != nil {
		return err
	}
	return writeElement(w, idCuePoint, inner.Bytes())
}

// WriteSeekHead writes the SeekHead master element pointing at the Info,
// Tracks, and Cues elements. Positions are byte offsets relative to the
// Segment body start.
func WriteSeekHead(ws writeSeeker, infoPos, tracksPos, cuesPos int64) error {
	var buf trackedBuf
	if err := writeSeekEntry(&buf, idInfo, uint64(infoPos)); err != nil {
		return err
	}
	if err := writeSeekEntry(&buf, idTracks, uint64(tracksPos)); err != nil {
		return err
	}
	if cuesPos >= 0 {
		if err := writeSeekEntry(&buf, idCues, uint64(cuesPos)); err != nil {
			return err
		}
	}
	if err := writeID(ws, idSeekHead); err != nil {
		return err
	}
	if err := writeSize(ws, uint64(buf.Len())); err != nil {
		return err
	}
	_, err := ws.Write(buf.Bytes())
	return err
}

func writeSeekEntry(w io.Writer, targetID uint32, pos uint64) error {
	var inner trackedBuf
	idBytes, err := ebml.EncodeElementID(targetID)
	if err != nil {
		return err
	}
	if err := writeBinary(&inner, idSeekID, idBytes); err != nil {
		return err
	}
	if err := writeUint(&inner, idSeekPosition, pos); err != nil {
		return err
	}
	return writeElement(w, idSeek, inner.Bytes())
}

// PatchSegmentSize overwrites the reserved Segment size VINT at the given
// offset with the actual segment body length. width is the reserved width
// (8 for the Segment element).
func PatchSegmentSize(ws writeSeeker, offset int64, width int, segLen uint64) error {
	return patchSizeAt(ws, offset, width, segLen)
}

// encodeElementIDBytes returns the VINT bytes for an Element ID.