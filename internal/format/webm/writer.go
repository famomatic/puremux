package webm

import (
	"encoding/binary"
	"io"
	"math"

	"famomatic/puremux/internal/format/ebml"
)

// writeID writes an EBML Element ID.
func writeID(w io.Writer, id uint32) error {
	b, err := ebml.EncodeElementID(id)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeSize writes an EBML size VINT.
func writeSize(w io.Writer, size uint64) error {
	b, err := ebml.EncodeVINT(size)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeSizeWidth writes a size VINT of a fixed width (for reserved sizes that
// will be patched later).
func writeSizeWidth(w io.Writer, size uint64, width int) error {
	b, err := ebml.EncodeVINTWidth(size, width)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeUint writes a uint payload of the minimal big-endian width needed.
func writeUint(w io.Writer, id uint32, v uint64) error {
	if v == 0 {
		return writeElement(w, id, []byte{0})
	}
	// minimal bytes
	var buf [8]byte
	n := 0
	tmp := v
	for tmp > 0 {
		buf[7-n] = byte(tmp)
		tmp >>= 8
		n++
	}
	return writeElement(w, id, buf[8-n:])
}

// writeFloat writes a 64-bit float element (Duration uses IEEE-754 double).
func writeFloat(w io.Writer, id uint32, f float64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(f))
	return writeElement(w, id, buf[:])
}

// writeString writes a UTF-8 string element (no null terminator).
func writeString(w io.Writer, id uint32, s string) error {
	return writeElement(w, id, []byte(s))
}

// writeBinary writes a raw byte payload element.
func writeBinary(w io.Writer, id uint32, b []byte) error {
	return writeElement(w, id, b)
}

// writeElement writes id + size + payload.
func writeElement(w io.Writer, id uint32, payload []byte) error {
	if err := writeID(w, id); err != nil {
		return err
	}
	if err := writeSize(w, uint64(len(payload))); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// reserveElement writes id + a reserved fixed-width size VINT (value 0),
// returning the offset of the size VINT so it can be patched on Close.
// The caller writes payload bytes afterward and patches the size.
func reserveElement(w writeSeeker, id uint32, sizeWidth int) (sizeOffset int64, err error) {
	if err := writeID(w, id); err != nil {
		return 0, err
	}
	sizeOffset, err = w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if err := writeSizeWidth(w, 0, sizeWidth); err != nil {
		return 0, err
	}
	return sizeOffset, nil
}

// writeSeeker is the sink the WebM muxer targets. It mirrors muxer.Writer
// but lives in the format package to avoid an import cycle.
type writeSeeker interface {
	io.Writer
	Seek(offset int64, whence int) (int64, error)
}