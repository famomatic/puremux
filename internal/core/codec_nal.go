package core

// h264Detector parses H.264/AVC NAL unit headers to detect an IDR slice
// (sync frame). A packet may carry multiple NAL units, separated either by
// an AVCC length prefix (a big-endian length, default 4 bytes) or by the
// Annex B start code (0x000001 / 0x00000001).
//
// H.264 NAL unit header is one byte (ITU-T H.264, section 7.3.1):
//
//	forbidden_zero_bit (1) | nal_ref_idc (2) | nal_unit_type (5)
//
// nal_unit_type == 5 is an IDR slice (a sync frame we can start decoding
// from). Only the header byte of each NAL is inspected; the slice data is
// never decoded (opacity invariant, ARCHITECTURE.md section 4).
type h264Detector struct{}

// h264 nal_unit_type values (ITU-T H.264, Table 7-1).
const (
	h264NalTypeNonIDR = 1
	h264NalTypeIDR    = 5
	h264NalTypeSPS    = 7
	h264NalTypePPS    = 8
)

func (h264Detector) IsKeyframe(data []byte) bool {
	return scanNALs(data, 1, h264NALIsKeyframe)
}

// h264NALIsKeyframe inspects a single NAL unit's first byte. It reads the
// nal_unit_type from the low 5 bits and reports true for IDR (5).
func h264NALIsKeyframe(nal []byte) bool {
	if len(nal) < 1 {
		return false
	}
	b := nal[0]
	if b&0x80 != 0 {
		// forbidden_zero_bit set: malformed NAL.
		return false
	}
	nalType := b & 0x1F
	return nalType == h264NalTypeIDR
}

// hevcDetector parses H.265/HEVC NAL unit headers to detect a sync frame.
//
// HEVC NAL unit header is two bytes (ITU-T H.265, section 7.3.1):
//
//	forbidden_zero_bit (1) | nal_unit_type (6) | nuh_layer_id (6) | nuh_temporal_id_plus1 (3)
//
// Sync (key) frame NAL types are the IRAP set: BLA_W_LP(16), BLA_W_RADL(17),
// BLA_N_LP(18), IDR_W_RADL(19), IDR_N_LP(20), CRA_NUT(21). Only the header
// bytes are inspected; the coded slice is never decoded (opacity invariant).
type hevcDetector struct{}

// hevc nal_unit_type values (ITU-T H.265, Table 7-1).
const (
	hevcNalTypeTrailN   = 0
	hevcNalTypeTrailR   = 1
	hevcNalTypeBLAWLP   = 16
	hevcNalTypeBLAWRADL = 17
	hevcNalTypeBLANLP   = 18
	hevcNalTypeIDRWRADL = 19
	hevcNalTypeIDRNLP   = 20
	hevcNalTypeCRANUT   = 21
)

func (hevcDetector) IsKeyframe(data []byte) bool {
	return scanNALs(data, 2, hevcNALIsKeyframe)
}

// hevcNALIsKeyframe inspects a NAL unit's first two header bytes. The
// nal_unit_type occupies bits [6:1] of byte 0 (after the forbidden bit).
func hevcNALIsKeyframe(nal []byte) bool {
	if len(nal) < 2 {
		return false
	}
	b0 := nal[0]
	if b0&0x80 != 0 {
		// forbidden_zero_bit set: malformed NAL.
		return false
	}
	nalType := (b0 >> 1) & 0x3F
	return nalType >= hevcNalTypeBLAWLP && nalType <= hevcNalTypeCRANUT
}

// scanNALs walks a packet payload and applies check to each NAL unit's
// header bytes. It returns true as soon as one NAL satisfies check (OR
// semantics: a packet is a keyframe if any of its NAL units is a sync frame).
//
// headerBytes is the number of NAL header bytes the check needs (1 for
// H.264, 2 for HEVC). The packet framing may be either:
//
//   - AVCC: each NAL is prefixed by a big-endian length (1, 2, or 4 bytes).
//     The most common MP4/WebM store uses 4-byte length prefixes.
//   - Annex B: NAL units are prefixed by a start code 0x000001 or 0x00000001.
//
// We auto-detect framing by scanning for start codes; if none are found we
// fall back to the AVCC length-prefix interpretation with the conventional
// 4-byte prefix.
func scanNALs(data []byte, headerBytes int, check func([]byte) bool) bool {
	if len(data) == 0 {
		return false
	}
	// Annex B path: look for start codes.
	if hasAnnexBStartCode(data) {
		return scanAnnexBNALs(data, headerBytes, check)
	}
	// AVCC path: 4-byte big-endian length prefixes.
	return scanAVCCNALs(data, headerBytes, check)
}

// hasAnnexBStartCode reports whether data begins with (or contains early) an
// Annex B start code 0x000001 / 0x00000001.
func hasAnnexBStartCode(data []byte) bool {
	for i := 0; i+3 <= len(data) && i < 4; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if i+2 < len(data) && data[i+2] == 1 {
				return true
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return true
			}
		}
	}
	return false
}

// scanAnnexBNALs splits on start codes and applies check to each NAL header.
func scanAnnexBNALs(data []byte, headerBytes int, check func([]byte) bool) bool {
	i := 0
	for i < len(data) {
		// Locate the next start code beginning at or after i.
		scLen := startCodeLenAt(data, i)
		if scLen == 0 {
			// Advance to the next zero byte that could begin a start code.
			next := nextZeroRun(data, i)
			if next < 0 {
				break
			}
			i = next
			continue
		}
		nalStart := i + scLen
		// Find the next start code to bound this NAL.
		nalEnd := findNextStartCode(data, nalStart)
		if nalEnd < 0 {
			nalEnd = len(data)
		}
		if nalStart+headerBytes <= nalEnd {
			if check(data[nalStart : nalStart+headerBytes]) {
				return true
			}
		}
		i = nalEnd
	}
	return false
}

// startCodeLenAt returns 3 or 4 if a start code (0x000001 / 0x00000001) begins
// at i, else 0.
func startCodeLenAt(data []byte, i int) int {
	if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
		if i+4 <= len(data) && data[i+3] == 0 {
			// 0x00000001 is also matched as a 4-byte start code, but the 3-byte
			// form (0x000001) is a strict prefix; treat the leading zero as part
			// of the run. We return 4 to consume the full 0x00000001.
			return 3 // 3-byte 0x000001 already found; the extra 0x00 was consumed above
		}
		return 3
	}
	if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
		return 4
	}
	return 0
}

// nextZeroRun returns the index of the next 0x00 byte at or after i, or -1.
func nextZeroRun(data []byte, i int) int {
	for ; i < len(data); i++ {
		if data[i] == 0 {
			return i
		}
	}
	return -1
}

// findNextStartCode returns the index of the next start code at or after
// from, or -1 if none.
func findNextStartCode(data []byte, from int) int {
	for i := from; i+3 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				return i
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// scanAVCCNALs walks NAL units prefixed by 4-byte big-endian lengths and
// applies check to each header.
func scanAVCCNALs(data []byte, headerBytes int, check func([]byte) bool) bool {
	i := 0
	for i+4 <= len(data) {
		length := int(data[i])<<24 | int(data[i+1])<<16 | int(data[i+2])<<8 | int(data[i+3])
		nalStart := i + 4
		nalEnd := nalStart + length
		if length <= 0 || nalEnd > len(data) {
			// Malformed length: stop scanning safely rather than panic.
			return false
		}
		if nalStart+headerBytes <= nalEnd {
			if check(data[nalStart : nalStart+headerBytes]) {
				return true
			}
		}
		i = nalEnd
	}
	return false
}
