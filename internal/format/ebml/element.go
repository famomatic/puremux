package ebml

import (
	"errors"
	"io"
)

// ElementID is an EBML Element ID: a VINT whose data bits are constrained
// to the Class range. Classes (RFC 8794 section 11.1):
//
//	Class   width   ID range (hex)
//	1      1       0x81 .. 0xFF      (top-level common)
//	2      2       0x407 .. 0x7FF
//	3      3       0x2067 .. 0x3FFFF
//	4      4       0x1067 .. 0x1FFFFFFF
//
// Element IDs use the same VINT marker scheme but the data is the ID itself
// (the marker bit IS part of the stored ID). IDs 1..4 bytes only.
const (
	MaxElementIDWidth = 4
)

// ErrElementIDInvalid is returned for an ID that is not a valid EBML class.
var ErrElementIDInvalid = errors.New("ebml: invalid Element ID")

// EncodeElementID encodes an Element ID to its VINT bytes.
//
// IMPORTANT: an EBML Element ID value INCLUDES the marker bit. The id is the
// full VINT bit pattern, so encoding is just emitting the id as the minimum
// number of big-endian bytes whose top bits match the class marker:
//
//	Class 1 (1 byte): 0x81 .. 0xFF            (top bit 1)
//	Class 2 (2 bytes): 0x4000 .. 0x7FFF        (top 2 bits 01)
//	Class 3 (3 bytes): 0x200000 .. 0x3FFFFF     (top 3 bits 001)
//	Class 4 (4 bytes): 0x10000000 .. 0x1FFFFFFF (top 4 bits 0001)
//
// The marker bit is NOT added on top of id; it is already part of id.
func EncodeElementID(id uint32) ([]byte, error) {
	switch {
	case id >= 0x81 && id <= 0xFF:
		return []byte{byte(id)}, nil
	case id >= 0x4000 && id <= 0x7FFF:
		return []byte{byte(id >> 8), byte(id)}, nil
	case id >= 0x200000 && id <= 0x3FFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}, nil
	case id >= 0x10000000 && id <= 0x1FFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}, nil
	}
	return nil, ErrElementIDInvalid
}

// DecodeElementID reads an Element ID from src, returning the id, width, and
// error. The marker bit and data form the id together.
func DecodeElementID(src []byte) (id uint32, width int, err error) {
	if len(src) < 1 {
		return 0, 0, ErrElementIDInvalid
	}
	w := VINTWidth(src[0])
	if w == 0 || w > MaxElementIDWidth {
		return 0, 0, ErrElementIDInvalid
	}
	if len(src) < w {
		return 0, 0, ErrElementIDInvalid
	}
	var v uint32
	for i := 0; i < w; i++ {
		v = (v << 8) | uint32(src[i])
	}
	return v, w, nil
}

// Element is a typed EBML element: ID + size + payload bytes.
type Element struct {
	ID      uint32
	Payload []byte
}

// EncodeElement writes id, a size VINT for len(payload), and the payload to w.
// For unknown-size elements use EncodeElementUnknownSize.
func EncodeElement(w io.Writer, id uint32, payload []byte) error {
	idBytes, err := EncodeElementID(id)
	if err != nil {
		return err
	}
	sizeBytes, err := EncodeVINT(uint64(len(payload)))
	if err != nil {
		return err
	}
	if _, err := w.Write(idBytes); err != nil {
		return err
	}
	if _, err := w.Write(sizeBytes); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// EncodeElementUnknownSize writes id followed by the unknown-size sentinel
// of the given width. No payload size is recorded; callers stream children.
func EncodeElementUnknownSize(w io.Writer, id uint32, width int) error {
	idBytes, err := EncodeElementID(id)
	if err != nil {
		return err
	}
	sizeBytes, err := EncodeVINTUnknown(width)
	if err != nil {
		return err
	}
	if _, err := w.Write(idBytes); err != nil {
		return err
	}
	_, err = w.Write(sizeBytes)
	return err
}
