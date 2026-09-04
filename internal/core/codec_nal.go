package core

import "encoding/binary"

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

// IsConfigOnly reports an AU carrying only non-VCL NALs (SPS/PPS/SEI/AUD…)
// with at least one parameter set — decoder configuration the Aligner must
// not lose to its pre-keyframe drop (CodecConfigOnlyDetector).
func (h264Detector) IsConfigOnly(data []byte) bool {
	hasParamSet, hasVCL, any := false, false, false
	forEachNAL(data, func(nal []byte) bool {
		if len(nal) < 1 || nal[0]&0x80 != 0 {
			return true
		}
		any = true
		switch nalType := nal[0] & 0x1F; {
		case nalType >= 1 && nalType <= 5: // coded slices (VCL)
			hasVCL = true
			return false
		case nalType == h264NalTypeSPS || nalType == h264NalTypePPS:
			hasParamSet = true
		}
		return true
	})
	return any && hasParamSet && !hasVCL
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
	// 22/23 are RSV_IRAP_VCL22/23 — reserved IRAP VCL types. Still IRAP for POC
	// reset and still carry no_output_of_prior_pics_flag (H.265 §7.3.6.1).
	hevcNalTypeRSVIRAPVCL23 = 23
)

func (hevcDetector) IsKeyframe(data []byte) bool {
	return scanNALs(data, 2, hevcNALIsKeyframe)
}

// IsConfigOnly reports an AU carrying only non-VCL NALs with at least one
// parameter set (VPS 32 / SPS 33 / PPS 34) — see CodecConfigOnlyDetector.
func (hevcDetector) IsConfigOnly(data []byte) bool {
	hasParamSet, hasVCL, any := false, false, false
	forEachNAL(data, func(nal []byte) bool {
		if !validHEVCNALHeader(nal) {
			return true
		}
		any = true
		switch nalType := (nal[0] >> 1) & 0x3F; {
		case nalType <= 31: // coded slices (VCL)
			hasVCL = true
			return false
		case nalType >= 32 && nalType <= 34: // VPS/SPS/PPS
			hasParamSet = true
		}
		return true
	})
	return any && hasParamSet && !hasVCL
}

// hevcNALIsKeyframe inspects a NAL unit's first two header bytes. The
// nal_unit_type occupies bits [6:1] of byte 0 (after the forbidden bit).
func hevcNALIsKeyframe(nal []byte) bool {
	if !validHEVCNALHeader(nal) {
		return false
	}
	nalType := (nal[0] >> 1) & 0x3F
	// H.265 requires TemporalId == 0 for IRAP VCL NAL units, so the
	// encoded nuh_temporal_id_plus1 field must be exactly one.
	return nalType >= hevcNalTypeBLAWLP && nalType <= hevcNalTypeCRANUT && nal[1]&0x07 == 1
}

func validHEVCNALHeader(nal []byte) bool {
	return len(nal) >= 2 && nal[0]&0x80 == 0 && nal[1]&0x07 != 0
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
	stop := false
	forEachNAL(data, func(nal []byte) bool {
		if len(nal) >= headerBytes && check(nal[:headerBytes]) {
			stop = true
			return false
		}
		return true
	})
	return stop
}

// forEachNAL walks every NAL unit in a packet payload (Annex-B or AVCC
// framing, auto-detected like scanNALs) and passes the FULL NAL bytes
// (header + payload, no start code / length prefix) to fn. Returning false
// from fn stops the walk. Malformed framing ends the walk safely.
func forEachNAL(data []byte, fn func(nal []byte) bool) {
	if len(data) == 0 {
		return
	}
	if hasAnnexBStartCode(data) {
		forEachAnnexBNAL(data, fn)
		return
	}
	forEachAVCCNAL(data, fn)
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

// forEachAnnexBNAL splits on start codes and passes each full NAL to fn.
func forEachAnnexBNAL(data []byte, fn func(nal []byte) bool) {
	i := 0
	for i < len(data) {
		// Locate the next start code beginning at or after i.
		scLen := startCodeLenAt(data, i)
		if scLen == 0 {
			// Advance to the next zero byte that could begin a start code.
			// Search from i+1: when data[i] is itself 0x00 but not the start of
			// a start code, nextZeroRun(data, i) would return i unchanged and
			// spin forever. Starting at i+1 guarantees forward progress.
			next := nextZeroRun(data, i+1)
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
		if nalStart < nalEnd {
			if !fn(data[nalStart:nalEnd]) {
				return
			}
		}
		i = nalEnd
	}
}

// startCodeLenAt returns 3 or 4 if a start code (0x000001 / 0x00000001) begins
// at i, else 0.
func startCodeLenAt(data []byte, i int) int {
	// Check the 4-byte form (0x00000001) first so it consumes the full
	// leading zero run; otherwise the 3-byte form (0x000001) follows.
	if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
		return 4
	}
	if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
		return 3
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

// forEachAVCCNAL walks NAL units prefixed by 4-byte big-endian lengths and
// passes each full NAL to fn.
func forEachAVCCNAL(data []byte, fn func(nal []byte) bool) {
	i := 0
	for i+4 <= len(data) {
		length := binary.BigEndian.Uint32(data[i : i+4])
		nalStart := i + 4
		if length == 0 || uint64(length) > uint64(len(data)-nalStart) {
			// Malformed length: stop scanning safely rather than panic.
			return
		}
		nalEnd := nalStart + int(length)
		if !fn(data[nalStart:nalEnd]) {
			return
		}
		i = nalEnd
	}
}
