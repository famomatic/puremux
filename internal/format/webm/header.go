package webm

import (
	"io"
	"time"
)

// Header is the bookkeeping the muxer needs to patch reserved fields on
// Close (ARCHITECTURE.md section 5.C graceful closer).
type Header struct {
	// Seekable indicates whether the sink supports Seek. When false the muxer
	// runs in streaming mode and omits Cues + leaves Duration unset.
	Seekable bool

	// SegmentStart is the byte offset where the Segment element body begins
	// (i.e. just after the Segment ID + size VINT).
	SegmentStart int64

	// SegmentSizeOff is the offset of the Segment's reserved size VINT.
	// Patched to the real Segment length on Close when seekable.
	SegmentSizeOff int64

	// DurationOff is the offset of the Duration element's size VINT (the
	// value is a float, so we patch the float payload, not the size). We
	// store the payload offset directly for simplicity.
	DurationPayloadOff int64
	HasDuration        bool

	// CuesStart is the offset where the Cues element begins, if written.
	CuesStart int64
	HasCues   bool

	// TracksEnd is the offset just past the Tracks element (cluster start).
	TracksEnd int64

	// FirstClusterOff records the offset of the first Cluster for SeekHead.
	FirstClusterOff int64
}

// WriteEBMLHeader writes the EBML header element (id 0x1A45DFA3) wrapping
// the WebM doctype declaration. The children are buffered so the master
// element can be written with a known size up front.
func WriteEBMLHeader(w io.Writer) error {
	var buf trackedBuf
	if err := writeUint(&buf, idEBMLVersion, 1); err != nil {
		return err
	}
	if err := writeUint(&buf, idEBMLReadVersion, 1); err != nil {
		return err
	}
	if err := writeUint(&buf, idEBMLMaxIDLength, 4); err != nil {
		return err
	}
	if err := writeUint(&buf, idEBMLMaxSizeLen, 8); err != nil {
		return err
	}
	if err := writeString(&buf, idDocType, "webm"); err != nil {
		return err
	}
	if err := writeUint(&buf, idDocTypeVersion, 4); err != nil {
		return err
	}
	if err := writeUint(&buf, idDocTypeReadVer, 2); err != nil {
		return err
	}
	return writeElement(w, idEBML, buf.Bytes())
}

// BeginSegment writes the Segment element header. For seekable sinks it
// reserves a fixed-width (8) size VINT to patch later; for non-seekable
// sinks it writes the unknown-size sentinel.
func BeginSegment(ws writeSeeker, seekable bool) (Header, error) {
	h := Header{Seekable: seekable}
	if err := writeID(ws, idSegment); err != nil {
		return h, err
	}
	off, err := ws.Seek(0, io.SeekCurrent)
	if err != nil {
		return h, err
	}
	h.SegmentSizeOff = off
	if seekable {
		// reserve 8-byte size, patched on Close.
		if err := writeSizeWidth(ws, 0, 8); err != nil {
			return h, err
		}
	} else {
		// unknown-size sentinel (8 bytes).
		b, _ := encodeUnknownSize(8)
		if _, err := ws.Write(b); err != nil {
			return h, err
		}
	}
	h.SegmentStart, err = ws.Seek(0, io.SeekCurrent)
	return h, err
}

// WriteInfo writes the Info element with TimecodeScale and a reserved
// Duration slot (seekable only). The Duration float payload offset is
// recorded in h for later patching.
func WriteInfo(ws writeSeeker, h *Header, timecodeScale uint64, now time.Time) error {
	// We must write Info as a master with known size. Compute payload then
	// write ID + size + payload in one shot (no patching needed for Info
	// size since we know it up front).
	var buf trackedBuf
	if err := writeUint(&buf, idTimestampScale, timecodeScale); err != nil {
		return err
	}
	if err := writeString(&buf, idMuxingApp, "puremux"); err != nil {
		return err
	}
	if err := writeString(&buf, idWritingApp, "puremux"); err != nil {
		return err
	}
	// DateUTC: nanoseconds since 2001-01-01T00:00:00Z (Matroska epoch).
	dateNS := now.UnixNano() - matroskaEpoch.UnixNano()
	if err := writeUint(&buf, idDateUTC, uint64(dateNS)); err != nil {
		return err
	}
	if h.Seekable {
		// reserve a Duration element: id + size(8) + 8 bytes float placeholder.
		if err := writeID(&buf, idDuration); err != nil {
			return err
		}
		if err := writeSizeWidth(&buf, 8, 1); err != nil { // 8-byte payload size
			return err
		}
		off, _ := ws.Seek(0, io.SeekCurrent)
		// Account for the Info element header we have not yet written.
		// We write the whole Info element atomically below, so the duration
		// payload offset = current ws pos + Info header + buf-so-far.
		// To keep it simple, flush buf as the Info element first, then patch
		// by re-seeking. Instead, we record the offset after writing.
		_ = off
		// Write 8 zero bytes as the float placeholder.
		if _, err := buf.Write(make([]byte, 8)); err != nil {
			return err
		}
		h.HasDuration = true
	}
	// Write Info element: id + size + buf bytes.
	if err := writeID(ws, idInfo); err != nil {
		return err
	}
	if err := writeSize(ws, uint64(buf.Len())); err != nil {
		return err
	}
	// Record Duration payload offset if applicable.
	if h.HasDuration {
		cur, _ := ws.Seek(0, io.SeekCurrent)
		// Duration payload is the last 8 bytes of buf; its absolute offset
		// is cur + (buf.Len() - 8).
		h.DurationPayloadOff = cur + int64(buf.Len()-8)
	}
	if _, err := ws.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

// matroskaEpoch is the Matroska/EBML date origin: 2001-01-01T00:00:00Z.
var matroskaEpoch = time.Unix(978307200, 0).UTC()

// encodeUnknownSize returns the unknown-size sentinel bytes of a width.
func encodeUnknownSize(width int) ([]byte, error) {
	// local import to avoid cycle: re-derive
	marker := byte(0x80 >> uint(width-1))
	out := make([]byte, width)
	out[0] = marker
	for i := 1; i < width; i++ {
		out[i] = 0xFF
	}
	if width > 1 {
		// set the byte0 data bits to 1 as well
		out[0] = marker | byte((1<<uint(8-width))-1)
	}
	return out, nil
}

// trackedBuf is a bytes.Buffer-like that tracks written length.
type trackedBuf struct {
	data []byte
}

func (b *trackedBuf) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *trackedBuf) Len() int      { return len(b.data) }
func (b *trackedBuf) Bytes() []byte { return b.data }