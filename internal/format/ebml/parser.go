package ebml

import (
	"errors"
	"io"
)

// ErrShortInput is returned when the input ends before a complete element.
var ErrShortInput = errors.New("ebml: input too short for element")

// Header is a decoded EBML element header: the ID, the offset of its size
// VINT, the size value, and the offset where the payload begins.
type Header struct {
	ID        uint32
	Size      uint64
	IDOffset  int // byte offset of the ID
	SizeOff   int // byte offset of the size VINT
	SizeWidth int // width of the size VINT in bytes
	Unknown   bool
}

// PayloadOffset returns the byte offset where this element's payload begins.
func (h Header) PayloadOffset() int {
	return h.SizeOff + h.SizeWidth
}

// ReadHeader parses an EBML element header (ID + size) at the current reader
// position. It does NOT consume the payload.
func ReadHeader(r io.Reader) (Header, error) {
	var idBuf [4]byte
	// Read 1 byte to determine ID width.
	if _, err := io.ReadFull(r, idBuf[:1]); err != nil {
		return Header{}, err
	}
	w := VINTWidth(idBuf[0])
	if w == 0 || w > MaxElementIDWidth {
		return Header{}, ErrElementIDInvalid
	}
	if _, err := io.ReadFull(r, idBuf[1:w]); err != nil {
		return Header{}, ErrShortInput
	}
	id, _, err := DecodeElementID(idBuf[:w])
	if err != nil {
		return Header{}, err
	}
	// Read size VINT: 1 byte to determine width.
	var sizeBuf [8]byte
	if _, err := io.ReadFull(r, sizeBuf[:1]); err != nil {
		return Header{}, ErrShortInput
	}
	sw := VINTWidth(sizeBuf[0])
	if sw == 0 {
		return Header{}, ErrVINTInvalid
	}
	if _, err := io.ReadFull(r, sizeBuf[1:sw]); err != nil {
		return Header{}, ErrShortInput
	}
	size, _, err := DecodeVINT(sizeBuf[:sw])
	if err != nil {
		return Header{}, err
	}
	return Header{
		ID:        id,
		Size:      size,
		SizeWidth: sw,
		Unknown:   IsUnknownSize(sizeBuf[:sw], sw),
	}, nil
}

// PatchSize overwrites the size VINT at the given offset in dst with the new
// size, using the SAME width as the existing VINT (which must have been
// reserved with that width). Returns ErrVINTOverflow if newSize does not fit
// the width.
//
// This is the core primitive for the muxer's graceful closer: reserve a
// fixed-width size VINT up front, then patch it with the real size on Close.
func PatchSize(dst []byte, offset, width int, newSize uint64) error {
	if offset < 0 || offset+width > len(dst) {
		return ErrShortInput
	}
	enc, err := EncodeVINTWidth(newSize, width)
	if err != nil {
		return err
	}
	copy(dst[offset:offset+width], enc)
	return nil
}
