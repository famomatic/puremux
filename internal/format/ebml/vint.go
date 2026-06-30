package ebml

import "errors"

// VINT (Variable-size INTeger) is EBML's length-prefixed integer encoding
// (RFC 8794 section 4). A VINT is a sequence of N bytes whose first byte
// carries a unary marker: the position of the leading 1-bit in byte 0 sets
// the total width. The remaining bits (after the marker) form the data,
// read MSB-first (big-endian) across the whole VINT.
//
// Widths and data-bit counts:
//
//	width  marker byte   data bits   max value
//	1     1xxxxxxx        7          (1<<7)-1   = 126
//	2     01xxxxxx........ 14        (1<<14)-1  = 16382
//	3     001xxxxx........  21        (1<<21)-1  = 2097150
//	4     0001xxxx........  28        (1<<28)-1  = 268435454
//	5     00001xxx........  35
//	6     000001xx........  42
//	7     0000001x........  49
//	8     00000001........  56   (marker occupies the whole first byte)
//
// Unknown-size sentinel: the data bits are all 1. e.g. width-1 unknown is
// 0xFF (marker 1 + data 1111111). Width-8 unknown is 0x01 0xFF..FF.

const (
	// MaxVINTWidth is the widest VINT allowed by EBML (8 bytes).
	MaxVINTWidth = 8
)

// ErrVINTOverflow is returned when a value cannot be encoded in the requested
// width, or in any width up to MaxVINTWidth.
var ErrVINTOverflow = errors.New("ebml: VINT value overflows requested width")

// ErrVINTInvalid is returned when decoding a byte sequence that is not a
// valid VINT (e.g. all-zero first byte, or truncated).
var ErrVINTInvalid = errors.New("ebml: invalid VINT")

// VINTWidth returns the encoded width in bytes implied by the first byte's
// leading-1 marker position. Returns 0 for an all-zero byte (invalid).
func VINTWidth(firstByte byte) int {
	for i := 0; i < 8; i++ {
		if firstByte&(0x80>>uint(i)) != 0 {
			return i + 1
		}
	}
	return 0
}

// vintDataBits returns the number of data bits available in a VINT of the
// given width (RFC 8794). byte 0 carries the marker at bit (8-width) and
// data in the bits below it; width 8 has its marker at bit 0 and no data
// bits in byte 0 (all 56 data bits live in bytes 1..7).
func vintDataBits(width int) uint {
	if width == MaxVINTWidth {
		return 56
	}
	return uint(8-width) + uint(8*(width-1))
}

// EncodeVINT encodes val into the narrowest width that holds it, using
// EBML's standard (non-unknown) encoding. The marker bit is set and the
// value is placed in the low data bits, MSB-first.
func EncodeVINT(val uint64) ([]byte, error) {
	if val == 0 {
		return []byte{0x80}, nil // width 1, marker 1, data 0 => 0x80
	}
	for width := 1; width <= MaxVINTWidth; width++ {
		dataBits := vintDataBits(width)
		if val>>dataBits != 0 {
			continue // does not fit in this width
		}
		out := make([]byte, width)
		// marker: set bit (8-width) of byte 0.
		out[0] = byte(0x80 >> uint(width-1))
		// data fills the remaining bits MSB-first.
		// The data occupies bits [dataBits-1 .. 0] of val; spread across the
		// VINT with the low dataBits of byte 0..width-1 (byte 0 has
		// 8-(width) leading marker/reserved bits + (7-(width-1)) = 8-width
		// ... simpler: byte0 holds top (7-(width-1)) data bits, then full bytes.
		topBits := uint(8 - width) // data bits living in byte 0
		_ = topBits
		// Place data: byte 0's low (8-width... no). Recompute cleanly below.
		encodeVINTInto(out, val, width)
		return out, nil
	}
	return nil, ErrVINTOverflow
}

// encodeVINTInto writes val into a width-byte VINT at out (out must be
// exactly width bytes). The marker bit (bit 8-width of byte 0) is set and
// the data is laid out MSB-first: byte 0 holds the high (8-width) data bits
// in its low part, and bytes 1..width-1 each hold 8 data bits.
func encodeVINTInto(out []byte, val uint64, width int) {
	marker := byte(0x80 >> uint(width-1))
	out[0] = marker
	// byte0 data bits = low (8-width) bits, but the value's top (8-width)
	// data bits land there. Total data bits = 7 + (width-1)*8.
	byte0DataBits := vintDataBits(width) - uint(8*(width-1))
	if byte0DataBits == 7 {
		// width 1: all 7 data bits live in byte 0.
		out[0] |= byte(val & 0x7F)
		return
	}
	if byte0DataBits > 0 {
		out[0] |= byte(val >> uint(8*(width-1)))
	}
	for i := 1; i < width; i++ {
		shift := uint(8 * (width - 1 - i))
		out[i] = byte(val >> shift)
	}
}

// EncodeVINTWidth encodes val into exactly the given width, padding the high
// data bits with zeros if val is smaller. Returns ErrVINTOverflow if val
// does not fit in the width's data bits. width must be 1..MaxVINTWidth.
func EncodeVINTWidth(val uint64, width int) ([]byte, error) {
	if width < 1 || width > MaxVINTWidth {
		return nil, ErrVINTInvalid
	}
	dataBits := vintDataBits(width)
	if val>>dataBits != 0 {
		return nil, ErrVINTOverflow
	}
	out := make([]byte, width)
	encodeVINTInto(out, val, width)
	return out, nil
}

// EncodeVINTUnknown encodes the EBML unknown-size sentinel at a given width.
// All data bits are set to 1. width must be 1..MaxVINTWidth.
func EncodeVINTUnknown(width int) ([]byte, error) {
	if width < 1 || width > MaxVINTWidth {
		return nil, ErrVINTInvalid
	}
	out := make([]byte, width)
	out[0] = byte(0x80 >> uint(width-1))
	// set all data bits to 1
	dataBits := vintDataBits(width)
	mask := uint64(1)<<dataBits - 1
	encodeVINTInto(out, mask, width)
	return out, nil
}

// DecodeVINT reads a VINT from src, returning its data value, the width in
// bytes consumed, and an error. A truncated or all-zero-leading input is
// invalid. Unknown-size values (all data bits set) are returned as the
// sentinel (data == mask); callers distinguish via IsUnknownSize.
func DecodeVINT(src []byte) (val uint64, width int, err error) {
	if len(src) < 1 {
		return 0, 0, ErrVINTInvalid
	}
	w := VINTWidth(src[0])
	if w == 0 {
		return 0, 0, ErrVINTInvalid
	}
	if len(src) < w {
		return 0, 0, ErrVINTInvalid
	}
	// byte 0 data bits: the low (8-width) bits hold the high data bits,
	// EXCEPT width 1 where marker is bit7 and data is bits 6..0 (7 bits).
	// mask = 0xFF >> width  (clears the top width marker/zero bits).
	//   width 1 => 0xFF>>1 = 0x7F (7 bits)  correct
	//   width 2 => 0xFF>>2 = 0x3F (6 bits)  correct
	mask := byte(0xFF >> uint(w))
	var v uint64
	v |= uint64(src[0] & mask) << uint(8*(w-1))
	for i := 1; i < w; i++ {
		v |= uint64(src[i]) << uint(8*(w-1-i))
	}
	return v, w, nil
}

// IsUnknownSize reports whether the VINT at src is the unknown-size
// sentinel (all data bits set). Requires src to be at least width long.
func IsUnknownSize(src []byte, width int) bool {
	if width < 1 || width > MaxVINTWidth || len(src) < width {
		return false
	}
	dataBits := vintDataBits(width)
	mask := uint64(1)<<dataBits - 1
	v, _, err := DecodeVINT(src[:width])
	if err != nil {
		return false
	}
	return v == mask
}