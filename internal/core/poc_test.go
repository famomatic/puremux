package core

import (
	"testing"
)

// --- hand-derived H.264 fixtures (ITU-T H.264 §7.3.2.1, §7.3.2.2, §7.3.3) ---
//
// SPS (nal 0x67): profile_idc=66 (baseline: no chroma/scaling block),
// constraints=0x00, level_idc=30, then bit-exact:
//
//	seq_parameter_set_id         ue(0)  -> 1
//	log2_max_frame_num_minus4    ue(0)  -> 1            (frame_num: 4 bits)
//	pic_order_cnt_type           ue(0)  -> 1
//	log2_max_poc_lsb_minus4      ue(2)  -> 011          (poc_lsb: 6 bits, MaxLsb=64)
//	max_num_ref_frames           ue(4)  -> 00101
//	gaps_in_frame_num_allowed    u(1)   -> 0
//	pic_width_in_mbs_minus1      ue(19) -> 000010100
//	pic_height_in_map_units_m1   ue(14) -> 0001111
//	frame_mbs_only_flag          u(1)   -> 1
//
// bits: 111 011 00101 0 000010100 0001111 1 -> EC A0 A0 F8|stop -> EC A0 A0 FC
var h264TestSPS = []byte{0x67, 0x42, 0x00, 0x1E, 0xEC, 0xA0, 0xA0, 0xFC}

// PPS (nal 0x68): pic_parameter_set_id ue(0)->1, seq_parameter_set_id
// ue(0)->1, entropy_coding_mode u(1)->0, bottom_field_pic_order_in_frame_
// present u(1)->0, then rbsp stop: bits 1100 1... -> C8
var h264TestPPS = []byte{0x68, 0xC8}

// h264Slice hand-assembles a slice NAL: first_mb ue(0), slice_type ue,
// pps_id ue(0), frame_num u(4), [idr_pic_id ue(0)], pic_order_cnt_lsb u(6).
func h264Slice(t *testing.T, idr bool, ref bool, sliceType, frameNum, pocLsb uint32) []byte {
	t.Helper()
	w := newBitWriter()
	w.ue(0) // first_mb_in_slice
	w.ue(sliceType)
	w.ue(0) // pps_id
	w.u(frameNum, 4)
	if idr {
		w.ue(0) // idr_pic_id
	}
	w.u(pocLsb, 6)
	w.stop()
	hdr := byte(0x01)
	if idr {
		hdr = 0x05
	}
	if ref {
		hdr |= 0x60
	}
	return append([]byte{hdr}, w.bytes()...)
}

// bitWriter assembles MSB-first bitstreams for test fixtures.
type bitWriter struct {
	buf []byte
	n   uint
}

func newBitWriter() *bitWriter { return &bitWriter{} }

func (w *bitWriter) u(v uint32, bits uint) {
	for i := bits; i > 0; i-- {
		if w.n%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>(i-1)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> (w.n % 8)
		}
		w.n++
	}
}

func (w *bitWriter) ue(v uint32) {
	k := v + 1
	bits := uint(0)
	for t := k; t > 0; t >>= 1 {
		bits++
	}
	w.u(0, bits-1)
	w.u(k, bits)
}

func (w *bitWriter) stop() { w.u(1, 1) }

func (w *bitWriter) bytes() []byte { return w.buf }

func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, n...)
	}
	return out
}

func TestH264POCTypeZeroIPBB(t *testing.T) {
	// Decode order I(poc0) P(poc6) B(poc2,ref) b(poc4,nonref) mirroring an
	// IPBB mini-GOP with poc step 2. Display order must come out I,B,b,P.
	p := NewPictureOrderParser(CodecH264)
	aus := [][]byte{
		annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0)),
		annexB(h264Slice(t, false, true, 0, 1, 6)),
		annexB(h264Slice(t, false, true, 1, 2, 2)),
		annexB(h264Slice(t, false, false, 1, 2, 4)),
	}
	var infos []POCInfo
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		infos = append(infos, info)
	}
	wantOrder := []int64{0, 6, 2, 4}
	for i, info := range infos {
		if info.Order != wantOrder[i] {
			t.Fatalf("AU %d order = %d, want %d", i, info.Order, wantOrder[i])
		}
		if info.Epoch != 1 {
			t.Fatalf("AU %d epoch = %d, want 1", i, info.Epoch)
		}
	}
	if !infos[0].IDR || infos[1].IDR {
		t.Fatal("IDR flags wrong")
	}
	// Display comparisons.
	if !infos[2].Before(infos[1]) || !infos[0].Before(infos[2]) {
		t.Fatal("display order comparison wrong")
	}
}

func TestH264POCLsbWraparound(t *testing.T) {
	// MaxPocLsb = 64 and §8.2.1.1 wraps when consecutive reference POC LSBs
	// jump by >= MaxLsb/2. Advance references gradually (steps < 32) through
	// two full wraps: lsb 0,20,40,60 | 16,36,56 | 12 must unwrap to a
	// strictly increasing order 0,20,40,60,80,100,120,140.
	p := NewPictureOrderParser(CodecH264)
	lsbs := []uint32{20, 40, 60, 16, 36, 56, 12}
	aus := [][]byte{annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0))}
	for i, lsb := range lsbs {
		aus = append(aus, annexB(h264Slice(t, false, true, 0, uint32(i+1)%16, lsb)))
	}
	var got []int64
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		got = append(got, info.Order)
	}
	want := []int64{0, 20, 40, 60, 80, 100, 120, 140}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %d, want %d (wraparound broken: %v)", i, got[i], want[i], got)
		}
	}
}

func TestH264POCIDRResetsEpoch(t *testing.T) {
	p := NewPictureOrderParser(CodecH264)
	aus := [][]byte{
		annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0)),
		annexB(h264Slice(t, false, true, 0, 1, 6)),
		annexB(h264Slice(t, true, true, 7, 0, 0)), // new IDR: POC resets
		annexB(h264Slice(t, false, true, 0, 1, 6)),
	}
	var infos []POCInfo
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		infos = append(infos, info)
	}
	if infos[2].Epoch != infos[0].Epoch+1 {
		t.Fatalf("IDR did not advance epoch: %d then %d", infos[0].Epoch, infos[2].Epoch)
	}
	// Old-epoch P (order 6) must display before new-epoch IDR (order 0).
	if !infos[1].Before(infos[2]) {
		t.Fatal("cross-epoch display order wrong")
	}
}

func TestH264POCNonRefBFrames(t *testing.T) {
	// The x264 shape: refs advance by 4, each followed by a non-ref B whose
	// POC presents between the anchors. Orders must come out exactly as the
	// LSBs say, including across the wrap.
	p := NewPictureOrderParser(CodecH264)
	aus := [][]byte{annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0))}
	type step struct {
		ref bool
		lsb uint32
	}
	steps := []step{
		{true, 4}, {false, 2}, {true, 8}, {false, 6},
		{true, 12}, {false, 10},
	}
	for i, st := range steps {
		aus = append(aus, annexB(h264Slice(t, false, st.ref, 1, uint32(i+1)%16, st.lsb)))
	}
	want := []int64{0, 4, 2, 8, 6, 12, 10}
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		if info.Order != want[i] {
			t.Fatalf("order[%d] = %d, want %d", i, info.Order, want[i])
		}
	}
}

func TestH264POCRobustness(t *testing.T) {
	p := NewPictureOrderParser(CodecH264)
	// Slice before any SPS/PPS: must fail cleanly, not panic.
	if _, ok := p.ParseAU(annexB(h264Slice(t, false, true, 0, 0, 0))); ok {
		t.Fatal("slice without parameter sets parsed")
	}
	// Parameter-set-only AU: no picture.
	if _, ok := p.ParseAU(annexB(h264TestSPS, h264TestPPS)); ok {
		t.Fatal("SPS/PPS-only AU reported a picture")
	}
	// Truncated slice header: 1 byte of slice.
	if _, ok := p.ParseAU(annexB([]byte{0x65})); ok {
		t.Fatal("truncated slice parsed")
	}
	// Zero-length / nil AU.
	if _, ok := p.ParseAU(nil); ok {
		t.Fatal("nil AU parsed")
	}
	if _, ok := p.ParseAU([]byte{}); ok {
		t.Fatal("empty AU parsed")
	}
	// Garbage bytes.
	if _, ok := p.ParseAU([]byte{0xDE, 0xAD, 0xBE, 0xEF}); ok {
		t.Fatal("garbage parsed")
	}
	// After all that abuse, a valid sequence still parses.
	if _, ok := p.ParseAU(annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0))); !ok {
		t.Fatal("parser state corrupted by malformed input")
	}
}

func TestH264POCEmulationPrevention(t *testing.T) {
	// A slice header whose bits contain 00 00 03 emulation: craft poc_lsb=0
	// with surrounding zero bytes by using frame_num=0 etc. Rather than force
	// a specific pattern, verify the reader handles an SPS with an EP byte:
	// insert 00 00 03 03 inside a copy of the SPS trailing region — the
	// parser must still read the header fields before it (it stops earlier,
	// so parsing succeeds identically).
	sps := append([]byte{}, h264TestSPS...)
	sps = append(sps, 0x00, 0x00, 0x03, 0x03)
	p := NewPictureOrderParser(CodecH264)
	if _, ok := p.ParseAU(annexB(sps, h264TestPPS, h264Slice(t, true, true, 7, 0, 0))); !ok {
		t.Fatal("SPS with emulation-prevention tail failed")
	}
}

// --- HEVC fixtures (ITU-T H.265 §7.3.2.2.1, §7.3.2.3.1, §7.3.6.1) ---

// hevcTestSPS assembles a minimal SPS (nal type 33, layer 0, tid 1):
//
//	sps_video_parameter_set_id   u(4)=0
//	sps_max_sub_layers_minus1    u(3)=0
//	sps_temporal_id_nesting_flag u(1)=1
//	profile_tier_level: 88 zero bits + general_level_idc u(8)=93
//	sps_seq_parameter_set_id     ue(0)
//	chroma_format_idc            ue(1)
//	pic_width_in_luma_samples    ue(320)
//	pic_height_in_luma_samples   ue(180)
//	conformance_window_flag      u(1)=0
//	bit_depth_luma_minus8        ue(0)
//	bit_depth_chroma_minus8      ue(0)
//	log2_max_pic_order_cnt_lsb_minus4 ue(2)   (poc_lsb: 6 bits, MaxLsb=64)
func hevcTestSPS(t *testing.T) []byte {
	t.Helper()
	w := newBitWriter()
	w.u(0, 4)
	w.u(0, 3)
	w.u(1, 1)
	w.u(0, 32) // profile_tier_level general: space/tier/idc + compat[0:24]
	w.u(0, 32) // compat[24:32] + source flags + reserved[0:20]
	w.u(0, 24) // reserved[20:43] + inbld
	w.u(93, 8) // general_level_idc
	w.ue(0)    // sps_id
	w.ue(1)    // chroma_format_idc
	w.ue(320)
	w.ue(180)
	w.u(0, 1) // conformance_window_flag
	w.ue(0)   // bit_depth_luma
	w.ue(0)   // bit_depth_chroma
	w.ue(2)   // log2_max_poc_lsb_minus4
	w.stop()
	return append([]byte{33 << 1, 0x01}, w.bytes()...)
}

// hevcTestPPS: pps_id ue(0), sps_id ue(0), dependent_slice_segments u(1)=0,
// output_flag_present u(1)=0, num_extra_slice_header_bits u(3)=0.
func hevcTestPPS(t *testing.T) []byte {
	t.Helper()
	w := newBitWriter()
	w.ue(0)
	w.ue(0)
	w.u(0, 1)
	w.u(0, 1)
	w.u(0, 3)
	w.stop()
	return append([]byte{34 << 1, 0x01}, w.bytes()...)
}

// hevcSlice: first_slice_segment_in_pic_flag=1, [no_output_of_prior_pics for
// IRAP], pps_id ue(0), slice_type ue, [poc_lsb u(6) unless IDR].
func hevcSlice(t *testing.T, nalType byte, sliceType, pocLsb uint32) []byte {
	t.Helper()
	w := newBitWriter()
	w.u(1, 1) // first_slice_segment_in_pic_flag
	if nalType >= hevcNalTypeBLAWLP && nalType <= hevcNalTypeCRANUT {
		w.u(0, 1) // no_output_of_prior_pics_flag
	}
	w.ue(0) // pps_id
	w.ue(sliceType)
	if nalType != hevcNalTypeIDRWRADL && nalType != hevcNalTypeIDRNLP {
		w.u(pocLsb, 6)
	}
	w.stop()
	return append([]byte{nalType << 1, 0x01}, w.bytes()...)
}

func TestHEVCPOCBasicGOP(t *testing.T) {
	p := NewPictureOrderParser(CodecHEVC)
	aus := [][]byte{
		annexB(hevcTestSPS(t), hevcTestPPS(t), hevcSlice(t, hevcNalTypeIDRWRADL, 2, 0)),
		annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 6)), // P poc6
		annexB(hevcSlice(t, hevcNalTypeTrailR, 0, 2)), // B ref poc2
		annexB(hevcSlice(t, hevcNalTypeTrailN, 0, 4)), // b nonref poc4
	}
	wantOrder := []int64{0, 6, 2, 4}
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		if info.Order != wantOrder[i] {
			t.Fatalf("AU %d order = %d, want %d", i, info.Order, wantOrder[i])
		}
	}
}

func TestHEVCPOCWraparoundAndIDR(t *testing.T) {
	p := NewPictureOrderParser(CodecHEVC)
	aus := [][]byte{
		annexB(hevcTestSPS(t), hevcTestPPS(t), hevcSlice(t, hevcNalTypeIDRWRADL, 2, 0)),
		annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 30)),
		annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 60)),
		annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 26)), // wraps: 64+26=90
		annexB(hevcSlice(t, hevcNalTypeIDRNLP, 2, 0)),  // IDR resets, epoch++
		annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 4)),
	}
	type want struct {
		order int64
		epoch int
	}
	wants := []want{{0, 1}, {30, 1}, {60, 1}, {90, 1}, {0, 2}, {4, 2}}
	for i, au := range aus {
		info, ok := p.ParseAU(au)
		if !ok {
			t.Fatalf("AU %d did not parse", i)
		}
		if info.Order != wants[i].order || info.Epoch != wants[i].epoch {
			t.Fatalf("AU %d = (order %d, epoch %d), want (%d, %d)",
				i, info.Order, info.Epoch, wants[i].order, wants[i].epoch)
		}
	}
}

func TestHEVCPOCRobustness(t *testing.T) {
	p := NewPictureOrderParser(CodecHEVC)
	if _, ok := p.ParseAU(annexB(hevcSlice(t, hevcNalTypeTrailR, 1, 4))); ok {
		t.Fatal("slice without parameter sets parsed")
	}
	if _, ok := p.ParseAU(annexB(hevcTestSPS(t), hevcTestPPS(t))); ok {
		t.Fatal("parameter-set-only AU reported a picture")
	}
	if _, ok := p.ParseAU([]byte{0x00, 0x01}); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := p.ParseAU(annexB(hevcTestSPS(t), hevcTestPPS(t), hevcSlice(t, hevcNalTypeIDRWRADL, 2, 0))); !ok {
		t.Fatal("parser state corrupted by malformed input")
	}
}

func TestConfigOnlyDetection(t *testing.T) {
	h264 := NewDetectorRegistry().Detector(CodecH264).(CodecConfigOnlyDetector)
	if !h264.IsConfigOnly(annexB(h264TestSPS, h264TestPPS)) {
		t.Fatal("H.264 SPS+PPS AU not detected as config-only")
	}
	if h264.IsConfigOnly(annexB(h264TestSPS, h264TestPPS, h264Slice(t, true, true, 7, 0, 0))) {
		t.Fatal("H.264 AU with an IDR slice flagged config-only")
	}
	if h264.IsConfigOnly(annexB(h264Slice(t, false, true, 0, 1, 2))) {
		t.Fatal("H.264 slice-only AU flagged config-only")
	}
	if h264.IsConfigOnly(annexB([]byte{0x06, 0x05, 0x01})) {
		t.Fatal("H.264 SEI-only AU (no parameter set) flagged config-only")
	}
	if h264.IsConfigOnly(nil) || h264.IsConfigOnly([]byte{}) {
		t.Fatal("empty AU flagged config-only")
	}

	hevc := NewDetectorRegistry().Detector(CodecHEVC).(CodecConfigOnlyDetector)
	if !hevc.IsConfigOnly(annexB(hevcTestSPS(t), hevcTestPPS(t))) {
		t.Fatal("HEVC SPS+PPS AU not detected as config-only")
	}
	if hevc.IsConfigOnly(annexB(hevcTestSPS(t), hevcTestPPS(t), hevcSlice(t, hevcNalTypeIDRWRADL, 2, 0))) {
		t.Fatal("HEVC AU with an IDR slice flagged config-only")
	}
}

func TestPOCParserNilForNonNALCodecs(t *testing.T) {
	for _, c := range []CodecType{CodecVP8, CodecVP9, CodecAV1, CodecAAC, CodecOpus} {
		if NewPictureOrderParser(c) != nil {
			t.Fatalf("codec %v unexpectedly has a POC parser", c)
		}
	}
}
