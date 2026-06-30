package core

// av1Detector parses the AV1 OBU (Open Bitstream Unit) headers in a packet
// to detect a sync frame (AV1 spec section 6.2, "OBU header" and
// "frame_header_obu").
//
// A packet may carry one or more OBUs. We scan OBUs until we find a
// FRAME_HEADER OBU; for it, frame_type == KEY_FRAME (0) and show_existing_frame
// handling is checked. frame_type occupies 2 bits after show_existing_frame
// in the frame header. A KEY_FRAME (0) is a sync frame.
//
// Only header bytes are inspected; the coded frame body is never decoded.
type av1Detector struct{}

// OBU types (AV1 spec, obu_type).
const (
	obuSequenceHeader = 1
	obuTemporalDelimiter = 2
	obuFrameHeader = 3
	obuTileGroup = 4
	obuMetadata = 5
	obuFrame = 6
	obuRedundantFrameHeader = 7
)

func (av1Detector) IsKeyframe(data []byte) bool {
	off := 0
	for off < len(data) {
		h := data[off]
		// obu_header: [7] forbidden (must be 0), [6:3] obu_type, [2] obu_extension_flag, [1] obu_has_size_field, [0] reserved.
		if h&0x80 != 0 {
			// forbidden bit set: malformed OBU.
			return false
		}
		obuType := (h >> 3) & 0x0F
		hasSize := (h >> 1) & 0x01
		off++

		// optional extension byte
		if (h>>2)&0x01 == 1 {
			off++ // skip obu_extension_header
			if off > len(data) {
				return false
			}
		}

		var payloadLen int
		if hasSize == 1 {
			if off >= len(data) {
				return false
			}
			size, n, ok := readLeb128(data[off:])
			if !ok {
				return false
			}
			off += n
			payloadLen = int(size)
		} else {
			payloadLen = len(data) - off
		}

		if payloadLen < 0 || off+payloadLen > len(data) {
			return false
		}

		switch obuType {
		case obuFrameHeader, obuFrame, obuRedundantFrameHeader:
			return isAV1FrameHeaderKeyframe(data[off : off+payloadLen])
		}

		off += payloadLen
	}
	return false
}

// isAV1FrameHeaderKeyframe reads the minimal frame_header fields:
//   frame_type (2 bits) where 0 = KEY_FRAME. show_existing_frame (1 bit)
//   precedes frame_type. We decode a few leb128-derived bits from the
//   frame header's first bytes.
func isAV1FrameHeaderKeyframe(frame []byte) bool {
	if len(frame) < 1 {
		return false
	}
	// frame_header_obu begins with show_existing_frame (1 bit).
	bitOff := 0
	showExisting, bits, ok := readBitsMSB(frame, bitOff, 1)
	if !ok {
		return false
	}
	bitOff += bits
	if showExisting == 1 {
		// show_existing_frame: refers to a previously-shown frame; not a new
		// sync frame we can rely on as an IDR for the aligner.
		return false
	}
	// frame_type (2 bits): 0=KEY, 1=INTER, 2=INTRA_ONLY, 3=S_FRAME.
	ft, _, ok := readBitsMSB(frame, bitOff, 2)
	if !ok {
		return false
	}
	return ft == 0
}

// readLeb128 decodes an unsigned LEB128 value from b, returning value, byte
// count consumed, and ok.
func readLeb128(b []byte) (val uint64, n int, ok bool) {
	var more uint8
	for i := 0; i < 8 && i < len(b); i++ {
		c := b[i]
		val |= uint64(c&0x7F) << (7 * i)
		n++
		more = c & 0x80
		if more == 0 {
			ok = true
			return
		}
	}
	return 0, 0, false
}

// readBitsMSB reads \width\ bits from frame starting at bitOff using AV1
// MSB-first bitpacking (AV1 spec read_bits reads the high bit first).
// bit 0 of bitOff refers to the most significant bit of byte 0. Returns
// value, bits consumed, ok.
func readBitsMSB(frame []byte, bitOff, width int) (val, bits int, ok bool) {
	if width <= 0 || width > 16 {
		return 0, 0, false
	}
	var acc uint32
	for i := 0; i < width; i++ {
		absBit := bitOff + i
		byteIdx := absBit / 8
		if byteIdx >= len(frame) {
			return 0, 0, false
		}
		bit := (frame[byteIdx] >> uint(7-(absBit%8))) & 0x01
		acc = (acc << 1) | uint32(bit)
	}
	return int(acc), width, true
}