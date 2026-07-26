package core

// hevcPOCParser computes each coded picture's PicOrderCntVal from HEVC
// SPS/PPS and slice segment headers (ITU-T H.265 §7.3.2.2.1, §7.3.2.3.1,
// §7.3.6.1, §8.3.1). Like the H.264 parser it caches in-band parameter sets
// and tracks the POC MSB across slice_pic_order_cnt_lsb wraparound.
type hevcPOCParser struct {
	sps map[uint32]*hevcSPS
	pps map[uint32]*hevcPPS

	// §8.3.1: MSB/LSB of prevTid0Pic — the previous decode-order picture
	// with TemporalId 0 that is not a RASL/RADL/sub-layer-non-reference pic.
	prevMsb int32
	prevLsb int32

	epoch    int
	sawFirst bool // first parsed picture ⇒ NoRaslOutputFlag=1 for a CRA
}

type hevcSPS struct {
	log2MaxPocLsb       uint
	separateColourPlane bool
}

type hevcPPS struct {
	spsID             uint32
	outputFlagPresent bool
	extraHeaderBits   uint
}

func newHEVCPOCParser() *hevcPOCParser {
	return &hevcPOCParser{
		sps: make(map[uint32]*hevcSPS),
		pps: make(map[uint32]*hevcPPS),
	}
}

// hevc NAL unit types beyond the keyframe set (ITU-T H.265, Table 7-1).
const (
	hevcNalTypeRADLN = 6
	hevcNalTypeRASLR = 9
	hevcNalTypeSPS   = 33
	hevcNalTypePPS   = 34
)

// ParseAU walks the AU's NAL units: parameter sets update the cache, the
// first VCL slice segment yields the picture's POC.
func (p *hevcPOCParser) ParseAU(au []byte) (POCInfo, bool) {
	var info POCInfo
	ok := false
	forEachNAL(au, func(nal []byte) bool {
		if len(nal) < 2 || nal[0]&0x80 != 0 {
			return true
		}
		nalType := (nal[0] >> 1) & 0x3F
		tid := int(nal[1]&0x07) - 1
		if tid < 0 {
			return true // nuh_temporal_id_plus1 == 0 is forbidden; malformed
		}
		switch {
		case nalType == hevcNalTypeSPS:
			p.parseSPS(nal[2:])
		case nalType == hevcNalTypePPS:
			p.parsePPS(nal[2:])
		case nalType <= 31: // VCL
			info, ok = p.parseSlice(nal[2:], nalType, tid)
			return false // first VCL slice decides the picture
		}
		return true
	})
	return info, ok
}

// skipHEVCProfileTierLevel consumes profile_tier_level(1, maxSubLayersMinus1)
// (§7.3.3): an 88-bit general block + level, then per-sub-layer blocks.
func skipHEVCProfileTierLevel(r *rbspBitReader, maxSubLayersMinus1 uint32) bool {
	// general_profile_space(2) tier(1) idc(5) compat(32) source flags(4)
	// reserved(43) inbld(1) = 88 bits, then general_level_idc(8).
	if _, ok := r.u(32); !ok {
		return false
	}
	if _, ok := r.u(32); !ok {
		return false
	}
	if _, ok := r.u(24); !ok {
		return false
	}
	if _, ok := r.u(8); !ok { // general_level_idc
		return false
	}
	if maxSubLayersMinus1 == 0 {
		return true
	}
	profilePresent := make([]bool, maxSubLayersMinus1)
	levelPresent := make([]bool, maxSubLayersMinus1)
	for i := range profilePresent {
		var ok bool
		if profilePresent[i], ok = r.flag(); !ok {
			return false
		}
		if levelPresent[i], ok = r.flag(); !ok {
			return false
		}
	}
	// reserved_zero_2bits alignment up to 8 sub-layers.
	if _, ok := r.u(uint(2 * (8 - maxSubLayersMinus1))); !ok {
		return false
	}
	for i := range profilePresent {
		if profilePresent[i] {
			if _, ok := r.u(32); !ok {
				return false
			}
			if _, ok := r.u(32); !ok {
				return false
			}
			if _, ok := r.u(24); !ok {
				return false
			}
		}
		if levelPresent[i] {
			if _, ok := r.u(8); !ok {
				return false
			}
		}
	}
	return true
}

// parseSPS extracts log2_max_pic_order_cnt_lsb (+ separate colour plane).
func (p *hevcPOCParser) parseSPS(rbsp []byte) {
	r := newRBSPBitReader(rbsp)
	if _, ok := r.u(4); !ok { // sps_video_parameter_set_id
		return
	}
	maxSub, ok := r.u(3)
	if !ok {
		return
	}
	if _, ok = r.flag(); !ok { // sps_temporal_id_nesting_flag
		return
	}
	if !skipHEVCProfileTierLevel(r, maxSub) {
		return
	}
	spsID, ok := r.ue()
	if !ok || spsID > 15 {
		return
	}
	var s hevcSPS
	chroma, ok := r.ue()
	if !ok {
		return
	}
	if chroma == 3 {
		if s.separateColourPlane, ok = r.flag(); !ok {
			return
		}
	}
	if _, ok = r.ue(); !ok { // pic_width_in_luma_samples
		return
	}
	if _, ok = r.ue(); !ok { // pic_height_in_luma_samples
		return
	}
	conf, ok := r.flag()
	if !ok {
		return
	}
	if conf {
		for range 4 {
			if _, ok = r.ue(); !ok {
				return
			}
		}
	}
	if _, ok = r.ue(); !ok { // bit_depth_luma_minus8
		return
	}
	if _, ok = r.ue(); !ok { // bit_depth_chroma_minus8
		return
	}
	l2poc, ok := r.ue()
	if !ok || l2poc > 12 {
		return
	}
	s.log2MaxPocLsb = uint(l2poc) + 4
	p.sps[spsID] = &s
}

// parsePPS extracts pps_id → (sps_id, output_flag_present, extra header bits).
func (p *hevcPOCParser) parsePPS(rbsp []byte) {
	r := newRBSPBitReader(rbsp)
	ppsID, ok := r.ue()
	if !ok || ppsID > 63 {
		return
	}
	spsID, ok := r.ue()
	if !ok || spsID > 15 {
		return
	}
	if _, ok = r.flag(); !ok { // dependent_slice_segments_enabled_flag
		return
	}
	ofp, ok := r.flag()
	if !ok {
		return
	}
	extra, ok := r.u(3)
	if !ok {
		return
	}
	p.pps[ppsID] = &hevcPPS{spsID: spsID, outputFlagPresent: ofp, extraHeaderBits: uint(extra)}
}

// parseSlice reads the slice segment header up to slice_pic_order_cnt_lsb and
// runs the §8.3.1 POC derivation.
func (p *hevcPOCParser) parseSlice(rbsp []byte, nalType byte, tid int) (POCInfo, bool) {
	idr := nalType == hevcNalTypeIDRWRADL || nalType == hevcNalTypeIDRNLP
	irap := nalType >= hevcNalTypeBLAWLP && nalType <= hevcNalTypeCRANUT
	bla := nalType >= hevcNalTypeBLAWLP && nalType <= hevcNalTypeBLANLP

	r := newRBSPBitReader(rbsp)
	firstSlice, ok := r.flag()
	if !ok || !firstSlice {
		// The first VCL NAL of an AU must open the picture; anything else is
		// a framing problem we refuse to guess about.
		return POCInfo{}, false
	}
	if irap {
		if _, ok = r.flag(); !ok { // no_output_of_prior_pics_flag
			return POCInfo{}, false
		}
	}
	ppsID, ok := r.ue()
	if !ok {
		return POCInfo{}, false
	}
	pps := p.pps[ppsID]
	if pps == nil {
		return POCInfo{}, false
	}
	sps := p.sps[pps.spsID]
	if sps == nil {
		return POCInfo{}, false
	}
	if pps.extraHeaderBits > 0 {
		if _, ok = r.u(pps.extraHeaderBits); !ok {
			return POCInfo{}, false
		}
	}
	if _, ok = r.ue(); !ok { // slice_type
		return POCInfo{}, false
	}
	if pps.outputFlagPresent {
		if _, ok = r.flag(); !ok { // pic_output_flag
			return POCInfo{}, false
		}
	}
	if sps.separateColourPlane {
		if _, ok = r.u(2); !ok { // colour_plane_id
			return POCInfo{}, false
		}
	}

	// §8.3.1: NoRaslOutputFlag=1 (POC MSB reset, and POC=0 for IDR) for every
	// IDR/BLA, and for a CRA that starts the bitstream.
	pocReset := idr || bla || (irap && !p.sawFirst)
	first := !p.sawFirst
	p.sawFirst = true

	var order int64
	if idr {
		// IDR slice headers carry no POC LSB; PicOrderCntVal is 0.
		p.prevMsb, p.prevLsb = 0, 0
		p.epoch++
		order = 0
	} else {
		lsb32, ok := r.u(sps.log2MaxPocLsb)
		if !ok {
			return POCInfo{}, false
		}
		lsb := int32(lsb32)
		maxLsb := int32(1) << sps.log2MaxPocLsb
		var msb int32
		if pocReset {
			msb = 0
			if bla || first {
				p.epoch++
			}
		} else {
			switch {
			case lsb < p.prevLsb && p.prevLsb-lsb >= maxLsb/2:
				msb = p.prevMsb + maxLsb
			case lsb > p.prevLsb && lsb-p.prevLsb > maxLsb/2:
				msb = p.prevMsb - maxLsb
			default:
				msb = p.prevMsb
			}
		}
		order = int64(msb) + int64(lsb)
		// prevTid0Pic update: TemporalId 0 and not RASL/RADL/sub-layer-non-ref.
		slnr := nalType < hevcNalTypeBLAWLP && nalType%2 == 0
		radlRasl := nalType >= hevcNalTypeRADLN && nalType <= hevcNalTypeRASLR
		if tid == 0 && !slnr && !radlRasl {
			p.prevMsb, p.prevLsb = msb, lsb
		}
	}
	return POCInfo{Order: order, Epoch: p.epoch, IDR: idr}, true
}
