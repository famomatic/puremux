package core

// h264POCParser computes each coded picture's PicOrderCnt from H.264 SPS/PPS
// and slice headers (ITU-T H.264 §7.3.2.1, §7.3.2.2, §7.3.3, §8.2.1). It
// caches parameter sets seen in-band and tracks the POC MSB across the
// pic_order_cnt_lsb wraparound, exactly as the decoding process specifies.
//
// Supported: pic_order_cnt_type 0 (the universal default; x264/x265, hardware
// encoders) and type 2 (display order == decode order by definition).
// pic_order_cnt_type 1 (rare, frame_num-cycle based) is reported as
// unsupported per AU so callers fall back to decode-order presentation.
type h264POCParser struct {
	sps map[uint32]*h264SPS
	pps map[uint32]*h264PPS

	// POC type 0 decoding state (§8.2.1.1): the MSB/LSB of the previous
	// picture in decode order that had nal_ref_idc != 0.
	prevMsb int32
	prevLsb int32

	epoch  int
	decCnt int64 // decode-order counter, used as Order for POC type 2
}

type h264SPS struct {
	pocType             uint32
	log2MaxPocLsb       uint // POC type 0 only
	log2MaxFrameNum     uint
	frameMbsOnly        bool
	separateColourPlane bool
}

type h264PPS struct {
	spsID uint32
	// bottom_field_pic_order_in_frame_present_flag (§7.3.2.2): when set, a
	// frame's slice header carries delta_pic_order_cnt_bottom.
	bottomFieldPOCPresent bool
}

func newH264POCParser() *h264POCParser {
	return &h264POCParser{
		sps: make(map[uint32]*h264SPS),
		pps: make(map[uint32]*h264PPS),
	}
}

// h264HighProfileIDCs lists profile_idc values whose SPS carries the
// chroma/bit-depth/scaling-matrix block before log2_max_frame_num (§7.3.2.1.1).
var h264HighProfileIDCs = map[uint32]bool{
	100: true, 110: true, 122: true, 244: true, 44: true, 83: true,
	86: true, 118: true, 128: true, 138: true, 139: true, 134: true, 135: true,
}

// ParseAU walks the AU's NAL units: parameter sets update the cache, and the
// first VCL slice yields the picture's POC.
func (p *h264POCParser) ParseAU(au []byte) (POCInfo, bool) {
	var info POCInfo
	ok := false
	forEachNAL(au, func(nal []byte) bool {
		if len(nal) < 1 || nal[0]&0x80 != 0 {
			return true
		}
		nalType := nal[0] & 0x1F
		switch nalType {
		case h264NalTypeSPS:
			p.parseSPS(nal[1:])
		case h264NalTypePPS:
			p.parsePPS(nal[1:])
		case h264NalTypeNonIDR, h264NalTypeIDR:
			refIdc := (nal[0] >> 5) & 0x3
			info, ok = p.parseSlice(nal[1:], nalType == h264NalTypeIDR, refIdc != 0)
			return false // first VCL slice decides the picture
		}
		return true
	})
	if ok {
		p.decCnt++
	}
	return info, ok
}

// parseSPS extracts the fields POC decoding needs; a malformed SPS is ignored
// (the cache keeps its previous entry, if any).
func (p *h264POCParser) parseSPS(rbsp []byte) {
	r := newRBSPBitReader(rbsp)
	profileIDC, ok := r.u(8)
	if !ok {
		return
	}
	if _, ok = r.u(16); !ok { // constraint flags + level_idc
		return
	}
	spsID, ok := r.ue()
	if !ok || spsID > 31 {
		return
	}
	var s h264SPS
	if h264HighProfileIDCs[profileIDC] {
		chroma, ok := r.ue()
		if !ok {
			return
		}
		if chroma == 3 {
			sep, ok := r.flag()
			if !ok {
				return
			}
			s.separateColourPlane = sep
		}
		if _, ok = r.ue(); !ok { // bit_depth_luma_minus8
			return
		}
		if _, ok = r.ue(); !ok { // bit_depth_chroma_minus8
			return
		}
		if _, ok = r.flag(); !ok { // qpprime_y_zero_transform_bypass_flag
			return
		}
		scaling, ok := r.flag()
		if !ok {
			return
		}
		if scaling {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := range lists {
				present, ok := r.flag()
				if !ok {
					return
				}
				if present {
					size := 16
					if i >= 6 {
						size = 64
					}
					if !skipH264ScalingList(r, size) {
						return
					}
				}
			}
		}
	}
	l2fn, ok := r.ue()
	if !ok || l2fn > 12 {
		return
	}
	s.log2MaxFrameNum = uint(l2fn) + 4
	s.pocType, ok = r.ue()
	if !ok || s.pocType > 2 {
		return
	}
	if s.pocType == 0 {
		l2poc, ok := r.ue()
		if !ok || l2poc > 12 {
			return
		}
		s.log2MaxPocLsb = uint(l2poc) + 4
	} else if s.pocType == 1 {
		if _, ok = r.flag(); !ok { // delta_pic_order_always_zero_flag
			return
		}
		if _, ok = r.se(); !ok { // offset_for_non_ref_pic
			return
		}
		if _, ok = r.se(); !ok { // offset_for_top_to_bottom_field
			return
		}
		n, ok := r.ue()
		if !ok || n > 255 {
			return
		}
		for range n {
			if _, ok = r.se(); !ok {
				return
			}
		}
	}
	if _, ok = r.ue(); !ok { // max_num_ref_frames
		return
	}
	if _, ok = r.flag(); !ok { // gaps_in_frame_num_value_allowed_flag
		return
	}
	if _, ok = r.ue(); !ok { // pic_width_in_mbs_minus1
		return
	}
	if _, ok = r.ue(); !ok { // pic_height_in_map_units_minus1
		return
	}
	fmo, ok := r.flag()
	if !ok {
		return
	}
	s.frameMbsOnly = fmo
	p.sps[spsID] = &s
}

// skipH264ScalingList consumes one scaling_list() (§7.3.2.1.1.1).
func skipH264ScalingList(r *rbspBitReader, size int) bool {
	lastScale, nextScale := int32(8), int32(8)
	for range size {
		if nextScale != 0 {
			delta, ok := r.se()
			if !ok {
				return false
			}
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
	return true
}

// parsePPS extracts pps_id → (sps_id, bottom_field POC flag).
func (p *h264POCParser) parsePPS(rbsp []byte) {
	r := newRBSPBitReader(rbsp)
	ppsID, ok := r.ue()
	if !ok || ppsID > 255 {
		return
	}
	spsID, ok := r.ue()
	if !ok || spsID > 31 {
		return
	}
	if _, ok = r.flag(); !ok { // entropy_coding_mode_flag
		return
	}
	bf, ok := r.flag()
	if !ok {
		return
	}
	p.pps[ppsID] = &h264PPS{spsID: spsID, bottomFieldPOCPresent: bf}
}

// parseSlice reads the slice header up to the POC fields and runs the
// §8.2.1.1 (type 0) picture order count decoding.
func (p *h264POCParser) parseSlice(rbsp []byte, idr, ref bool) (POCInfo, bool) {
	r := newRBSPBitReader(rbsp)
	if _, ok := r.ue(); !ok { // first_mb_in_slice
		return POCInfo{}, false
	}
	if _, ok := r.ue(); !ok { // slice_type
		return POCInfo{}, false
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
	if sps.separateColourPlane {
		if _, ok = r.u(2); !ok { // colour_plane_id
			return POCInfo{}, false
		}
	}
	if _, ok = r.u(sps.log2MaxFrameNum); !ok { // frame_num
		return POCInfo{}, false
	}
	fieldPic := false
	if !sps.frameMbsOnly {
		if fieldPic, ok = r.flag(); !ok {
			return POCInfo{}, false
		}
		if fieldPic {
			if _, ok = r.flag(); !ok { // bottom_field_flag
				return POCInfo{}, false
			}
		}
	}
	if idr {
		if _, ok = r.ue(); !ok { // idr_pic_id
			return POCInfo{}, false
		}
		// §8.2.1: an IDR picture resets the picture order count.
		p.prevMsb, p.prevLsb = 0, 0
		p.epoch++
	}
	switch sps.pocType {
	case 2:
		// POC type 2: output order == decode order by definition (§8.2.1.3).
		return POCInfo{Order: p.decCnt, Epoch: p.epoch, IDR: idr}, true
	case 1:
		// Unsupported (frame_num-cycle POC); caller falls back per AU.
		return POCInfo{}, false
	}
	lsb32, ok := r.u(sps.log2MaxPocLsb)
	if !ok {
		return POCInfo{}, false
	}
	lsb := int32(lsb32)
	maxLsb := int32(1) << sps.log2MaxPocLsb
	// §8.2.1.1 PicOrderCntMsb derivation (wraparound tracking).
	var msb int32
	switch {
	case lsb < p.prevLsb && p.prevLsb-lsb >= maxLsb/2:
		msb = p.prevMsb + maxLsb
	case lsb > p.prevLsb && lsb-p.prevLsb > maxLsb/2:
		msb = p.prevMsb - maxLsb
	default:
		msb = p.prevMsb
	}
	top := int64(msb) + int64(lsb)
	order := top
	if pps.bottomFieldPOCPresent && !fieldPic {
		deltaBottom, ok := r.se()
		if !ok {
			return POCInfo{}, false
		}
		// A frame's POC is min(top, bottom) (§8.2.1); bottom = top + delta.
		if deltaBottom < 0 {
			order = top + int64(deltaBottom)
		}
	}
	if ref {
		p.prevMsb, p.prevLsb = msb, lsb
	}
	return POCInfo{Order: order, Epoch: p.epoch, IDR: idr}, true
}
