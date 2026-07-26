package core

// PictureOrderParser derives display-order information from compressed video
// access units delivered in decode order. It is the second sanctioned
// codec-byte-probing interface next to CodecKeyframeDetector (§5.A): it reads
// parameter-set and slice HEADER fields only (picture order count), never
// slice data.
//
// Implementations are stateful (they cache SPS/PPS and track POC wraparound),
// so obtain one instance per elementary stream via NewPictureOrderParser and
// feed it every AU of that stream in decode order. Not safe for concurrent
// use.
type PictureOrderParser interface {
	// ParseAU inspects one access unit and returns its picture-order info.
	//
	// ok=false means the AU carries no parseable coded picture: a parameter-
	// set/SEI-only AU, a malformed or truncated slice header, or a POC
	// variant the parser does not support. Callers must fall back to
	// decode-order presentation for such AUs; the parser keeps its state
	// consistent so later AUs still parse.
	ParseAU(au []byte) (POCInfo, bool)
}

// POCInfo places one coded picture in display order. Display order across a
// whole stream is (Epoch, Order) lexicographically: Epoch increments at every
// IDR/IRAP that resets the picture order count, and Order is the unwrapped
// POC value within that epoch. Order values are only meaningful relative to
// each other — codecs step them by 2 per frame, hierarchies leave gaps.
type POCInfo struct {
	Order int64
	Epoch int
	IDR   bool
}

// Before reports whether picture a displays before picture b.
func (a POCInfo) Before(b POCInfo) bool {
	if a.Epoch != b.Epoch {
		return a.Epoch < b.Epoch
	}
	return a.Order < b.Order
}

// NewPictureOrderParser returns a fresh picture-order parser for the codec,
// or nil when the codec has no bitstream-level display reordering (every
// codec but H.264/HEVC here: VPx/AV1 in this muxer's profiles present in
// decode order).
func NewPictureOrderParser(c CodecType) PictureOrderParser {
	switch c {
	case CodecH264:
		return newH264POCParser()
	case CodecHEVC:
		return newHEVCPOCParser()
	}
	return nil
}
