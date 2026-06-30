package webm

import (
	"encoding/binary"
)

// SimpleBlock flags (Matroska spec, Block header byte 4).
const (
	flagKeyframe       = 0x80
	flagInvisible       = 0x08
	flagLacing          = 0x06
	flagDiscardable    = 0x01
)

// EncodeSimpleBlock serializes a SimpleBlock payload (without the 0xA3
// element wrapper). Layout (Matroska spec):
//
//	TrackNumber  VINT (variable, 1+ bytes)
//	Timecode     int16 big-endian (relative to Cluster, -32768..32767ms)
//	Flags        1 byte (keyframe bit etc.)
//	Payload      opaque bytes
//
// The Cluster-relative timecode is a signed int16 of milliseconds when
// TimecodeScale is 1_000_000 (the WebM default). Callers MUST pre-convert
// packet PTS to that relative value.
func EncodeSimpleBlock(trackNum uint64, relTimecode int16, keyframe bool, payload []byte) []byte {
	// TrackNumber VINT: for SimpleBlock it is a "VINT" but uses the raw
	// width-1 form (1 byte) for track numbers 1..126, and width-2 for
	// 127..16383 with the same marker scheme as element-size VINTs.
	tn := encodeTrackNumber(trackNum)
	out := make([]byte, 0, len(tn)+3+len(payload))
	out = append(out, tn...)
	var tc [2]byte
	binary.BigEndian.PutUint16(tc[:], uint16(relTimecode))
	out = append(out, tc[0], tc[1])
	flags := byte(0)
	if keyframe {
		flags |= flagKeyframe
	}
	out = append(out, flags)
	out = append(out, payload...)
	return out
}

// encodeTrackNumber encodes a Matroska Block track number as a VINT. Track
// numbers 1..126 use width 1 (0x80 | n), 127..16383 use width 2.
func encodeTrackNumber(n uint64) []byte {
	if n == 0 {
		return []byte{0x80} // track 0 is invalid; encode as 0 width-1
	}
	if n <= 126 {
		return []byte{0x80 | byte(n)}
	}
	// width 2: marker bit6, 14 data bits.
	return []byte{0x40 | byte(n>>8), byte(n)}
}
