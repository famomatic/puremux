package core

// rbspBitReader reads bit fields from a NAL unit payload, transparently
// removing the emulation-prevention bytes (00 00 03 -> 00 00) that Annex-B
// and AVCC NAL units insert into the raw byte sequence payload (ITU-T H.264
// §7.4.1.1 / H.265 §7.4.2). Parsers use it to read the few header fields the
// architecture permits inspecting (§5.A); it never decodes slice data.
//
// All read methods return ok=false once the reader has run out of bits (or a
// syntax element overflows its type); callers must check ok and abandon the
// parse — a truncated or malformed NAL must never panic.
type rbspBitReader struct {
	data []byte
	pos  int  // next byte index
	cur  byte // byte currently being bit-consumed
	bit  uint // bits remaining in cur
	zero int  // count of consecutive 0x00 bytes consumed (for EP detection)
}

func newRBSPBitReader(data []byte) *rbspBitReader {
	return &rbspBitReader{data: data}
}

// nextByte consumes one payload byte, skipping emulation-prevention bytes.
func (r *rbspBitReader) nextByte() (byte, bool) {
	if r.pos >= len(r.data) {
		return 0, false
	}
	b := r.data[r.pos]
	if b == 0x03 && r.zero >= 2 {
		// Emulation-prevention byte: skip it and read the following byte.
		r.pos++
		r.zero = 0
		if r.pos >= len(r.data) {
			return 0, false
		}
		b = r.data[r.pos]
	}
	r.pos++
	if b == 0x00 {
		r.zero++
	} else {
		r.zero = 0
	}
	return b, true
}

// u reads an n-bit unsigned field (n <= 32).
func (r *rbspBitReader) u(n uint) (uint32, bool) {
	var v uint32
	for range n {
		if r.bit == 0 {
			b, ok := r.nextByte()
			if !ok {
				return 0, false
			}
			r.cur = b
			r.bit = 8
		}
		r.bit--
		v = v<<1 | uint32((r.cur>>r.bit)&1)
	}
	return v, true
}

// flag reads a single bit.
func (r *rbspBitReader) flag() (bool, bool) {
	v, ok := r.u(1)
	return v == 1, ok
}

// ue reads an unsigned Exp-Golomb value (H.264 §9.1 / H.265 §9.2).
func (r *rbspBitReader) ue() (uint32, bool) {
	zeros := 0
	for {
		b, ok := r.u(1)
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			// A legal header field never needs this; treat as malformed.
			return 0, false
		}
	}
	if zeros == 0 {
		return 0, true
	}
	rest, ok := r.u(uint(zeros))
	if !ok {
		return 0, false
	}
	return (1<<uint(zeros) - 1) + rest, true
}

// se reads a signed Exp-Golomb value: k -> (-1)^(k+1) * ceil(k/2).
func (r *rbspBitReader) se() (int32, bool) {
	k, ok := r.ue()
	if !ok {
		return 0, false
	}
	if k%2 == 1 {
		return int32(k/2) + 1, true
	}
	return -int32(k / 2), true
}
