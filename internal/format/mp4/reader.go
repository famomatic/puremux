package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/bits"

	"github.com/famomatic/puremux/internal/core"
)

// Track is a track parsed from the input MP4 moov/trak box.
type Track struct {
	Number          int
	ID              uint32
	Codec           core.CodecType
	IsVideo         bool
	Width           int
	Height          int
	Channels        int
	SampleRate      float64
	Timescale       uint32
	Duration        uint64
	Language        string
	CodecConfigType string
	CodecConfig     []byte
}

func timestampLess(left int64, leftScale uint32, right int64, rightScale uint32) bool {
	if leftScale == 0 || rightScale == 0 {
		return left < right
	}
	if left < 0 && right >= 0 {
		return true
	}
	if left >= 0 && right < 0 {
		return false
	}
	lHi, lLo := bits.Mul64(unsignedInt64(left), uint64(rightScale))
	rHi, rLo := bits.Mul64(unsignedInt64(right), uint64(leftScale))
	lessMagnitude := lHi < rHi || (lHi == rHi && lLo < rLo)
	if left < 0 {
		return !lessMagnitude && (lHi != rHi || lLo != rLo)
	}
	return lessMagnitude
}

func unsignedInt64(value int64) uint64 {
	if value >= 0 {
		return uint64(value)
	}
	return uint64(-(value + 1)) + 1
}

// Sample carries one opaque compressed sample with exact media-time ticks.
type Sample struct {
	TrackNum int
	// AbsMs is the compatibility view of PTS, rounded to milliseconds.
	AbsMs     uint64
	DTS       int64
	PTS       int64
	Duration  int64
	Timescale uint32
	Position  int64
	Keyframe  bool
	Data      []byte
}

// SeekNS positions every progressive track at the sync point selected from
// trackNumber and returns that point in nanoseconds.
func (rd *Reader) SeekNS(trackNumber int, targetNS int64) (int64, error) {
	return rd.SeekNSWithFlags(trackNumber, targetNS, true, false)
}

// SeekNSWithFlags positions every track relative to an eligible sample on
// trackNumber. With backward false it selects the earliest sample at or after
// targetNS; any permits non-sync video samples.
func (rd *Reader) SeekNSWithFlags(trackNumber int, targetNS int64, backward, any bool) (int64, error) {
	actual, scale, err := rd.seekScaled(trackNumber, targetNS, 1_000_000_000, backward, any)
	return scaleToNS(actual, scale), err
}

// SeekTicksWithFlags preserves the selected track's exact integer clock.
func (rd *Reader) SeekTicksWithFlags(trackNumber int, target int64, backward, any bool) (int64, error) {
	for _, track := range rd.tracks {
		if track.info.Number == trackNumber {
			actual, _, err := rd.seekScaled(trackNumber, target, track.timescale, backward, any)
			return actual, err
		}
	}
	return 0, ErrCorrupt
}

func (rd *Reader) seekScaled(trackNumber int, target int64, targetScale uint32, backward, any bool) (int64, uint32, error) {
	if target < 0 {
		target = 0
	}
	if len(rd.fragments) > 0 {
		chosen := -1
		for pass := 0; pass < 2 && chosen < 0; pass++ {
			for i, sample := range rd.fragments {
				if sample.track.info.Number != trackNumber || (!any && sample.track.info.IsVideo && !sample.keyframe) {
					continue
				}
				before := timestampLess(sample.pts, sample.track.timescale, target, targetScale)
				after := timestampLess(target, targetScale, sample.pts, sample.track.timescale)
				if pass == 0 && ((backward && after) || (!backward && before)) {
					continue
				}
				if chosen < 0 {
					chosen = i
					continue
				}
				wantLater := backward != (pass == 1)
				if (wantLater && sample.pts > rd.fragments[chosen].pts) || (!wantLater && sample.pts < rd.fragments[chosen].pts) {
					chosen = i
				}
			}
		}
		if chosen < 0 {
			return 0, 0, errors.New("mp4: no eligible seek sample")
		}
		selected := rd.fragments[chosen]
		rd.fragmentCursor = chosen
		for rd.fragmentCursor > 0 {
			previous := rd.fragments[rd.fragmentCursor-1]
			if timestampLess(previous.dts, previous.track.timescale, selected.dts, selected.track.timescale) {
				break
			}
			rd.fragmentCursor--
		}
		return selected.pts, selected.track.timescale, nil
	}
	var selected *trackState
	for _, track := range rd.tracks {
		if track.info.Number == trackNumber {
			selected = track
			break
		}
	}
	if selected == nil {
		return 0, 0, ErrCorrupt
	}
	index, actual, err := selected.findSeekIndexScaled(target, targetScale, selected.info.IsVideo && !any, backward)
	if err != nil {
		return 0, 0, err
	}
	for _, track := range rd.tracks {
		trackIndex := index
		if track != selected {
			trackIndex, _, err = track.findSeekIndexScaled(actual, selected.timescale, track.info.IsVideo && !any, true)
			if err != nil {
				return 0, 0, err
			}
		}
		if err := track.setSampleIndex(trackIndex); err != nil {
			return 0, 0, err
		}
	}
	rd.inited = true
	return actual, selected.timescale, nil
}

func (t *trackState) findSeekIndex(targetNS int64, keyOnly, backward bool) (uint32, int64, error) {
	return t.findSeekIndexScaled(targetNS, 1_000_000_000, keyOnly, backward)
}

func (t *trackState) findSeekIndexScaled(target int64, targetScale uint32, keyOnly, backward bool) (uint32, int64, error) {
	if err := t.initCursor(); err != nil {
		return 0, 0, err
	}
	var chosenState, fallbackState trackState
	found, haveFallback := false, false
	for t.consumed < t.totalSamples {
		if !t.peekNext() {
			if t.cursorErr != nil {
				return 0, 0, t.cursorErr
			}
			break
		}
		if !keyOnly || t.peek.keyframe {
			pts := t.peek.pts
			eligible := !timestampLess(target, targetScale, pts, t.timescale)
			if !backward {
				eligible = !timestampLess(pts, t.timescale, target, targetScale)
			}
			if eligible && (!found || (backward && pts > chosenState.peek.pts) || (!backward && pts < chosenState.peek.pts)) {
				chosenState = *t
				found = true
			}
			if !haveFallback || (backward && pts < fallbackState.peek.pts) || (!backward && pts > fallbackState.peek.pts) {
				fallbackState = *t
				haveFallback = true
			}
		}
		t.hasPeek = false
		t.consumed++
		t.advancePast(t.peek)
	}
	if !found {
		if !haveFallback {
			return 0, 0, errors.New("mp4: no eligible seek sample")
		}
		chosenState = fallbackState
	}
	*t = chosenState
	t.hasPeek = true
	return t.consumed, t.peek.pts, nil
}

func (t *trackState) setSampleIndex(index uint32) error {
	if t.hasPeek && t.consumed == index {
		return nil
	}
	if err := t.initCursor(); err != nil {
		return err
	}
	for t.consumed < index {
		if !t.peekNext() {
			return ErrCorrupt
		}
		t.hasPeek = false
		t.consumed++
		t.advancePast(t.peek)
	}
	return nil
}

func scaleToNS(value int64, scale uint32) int64 {
	if scale == 0 {
		return 0
	}
	hi, lo := bits.Mul64(unsignedInt64(value), 1_000_000_000)
	if hi >= uint64(scale) {
		if value < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	quotient, _ := bits.Div64(hi, lo, uint64(scale))
	if value < 0 {
		if quotient >= uint64(1)<<63 {
			return math.MinInt64
		}
		return -int64(quotient)
	}
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}

// NewReader wraps an MP4 input stream. The reader must implement
// io.ReadSeeker; MP4 sample tables reference absolute mdat offsets.
func NewReader(r io.Reader) (*Reader, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return nil, ErrNotSeekable
	}
	rd := &Reader{rs: rs, metadata: make(map[string]string), trex: make(map[uint32]trackDefaults)}
	if err := rd.parse(); err != nil {
		return nil, err
	}
	if len(rd.tracks) == 0 {
		return nil, ErrCorrupt
	}
	return rd, nil
}

// NextSample returns the next media sample in merged absolute-time order
// across all tracks, or io.EOF when exhausted.
func (rd *Reader) NextSample() (*Sample, error) {
	if len(rd.fragments) > 0 {
		if rd.fragmentCursor >= len(rd.fragments) {
			return nil, io.EOF
		}
		s := rd.fragments[rd.fragmentCursor]
		if _, err := rd.rs.Seek(s.off, io.SeekStart); err != nil {
			return nil, err
		}
		data := make([]byte, s.size)
		if _, err := io.ReadFull(rd.rs, data); err != nil {
			return nil, err
		}
		rd.fragmentCursor++
		absMs := uint64(0)
		if s.pts > 0 {
			absMs = timescaleToMs(uint64(s.pts), s.track.timescale)
		}
		return &Sample{TrackNum: s.track.info.Number, AbsMs: absMs, DTS: s.dts, PTS: s.pts, Duration: s.duration, Timescale: s.track.timescale, Position: s.off, Keyframe: s.keyframe, Data: data}, nil
	}
	// Prime streaming cursors on first call (O(tracks) work, no sample array).
	if !rd.inited {
		if err := rd.initCursors(); err != nil {
			return nil, err
		}
		rd.inited = true
	}
	// Find the track whose next (peeked) sample has the smallest absolute time.
	pick := -1
	var bestDTS int64
	var bestScale uint32
	for i, t := range rd.tracks {
		// Lazily compute the peek for this track if missing.
		if !t.hasPeek {
			if !t.peekNext() {
				if t.cursorErr != nil {
					return nil, t.cursorErr
				}
				continue // track exhausted
			}
			t.hasPeek = true
		}
		if pick < 0 || timestampLess(t.peek.dts, t.timescale, bestDTS, bestScale) {
			pick = i
			bestDTS = t.peek.dts
			bestScale = t.timescale
		}
	}
	if pick < 0 {
		return nil, io.EOF
	}
	t := rd.tracks[pick]
	s := t.peek
	if _, err := rd.rs.Seek(s.off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, s.size)
	if _, err := io.ReadFull(rd.rs, buf); err != nil {
		return nil, err
	}
	// Commit cursor state only after the sample bytes were read successfully.
	t.hasPeek = false
	t.consumed++
	t.advancePast(s)
	return &Sample{
		TrackNum:  t.info.Number,
		AbsMs:     s.absMs,
		DTS:       s.dts,
		PTS:       s.pts,
		Duration:  s.duration,
		Timescale: t.timescale,
		Position:  s.off,
		Keyframe:  s.keyframe,
		Data:      buf,
	}, nil
}

// parse walks the top-level boxes (ftyp, moov, mdat).
func (rd *Reader) parse() error {
	start, err := rd.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	end, err := rd.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := rd.rs.Seek(start, io.SeekStart); err != nil {
		return err
	}
	for {
		off, err := rd.rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if off == end {
			break
		}
		if off > end {
			return ErrCorrupt
		}
		b, err := readBox(rd.rs)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if b.size == 0 {
			b.size = end - off
			b.payload = b.size - int64(b.hdrSize)
		}
		if b.size > end-off {
			return io.ErrUnexpectedEOF
		}
		if b.size < int64(b.hdrSize) {
			return ErrCorrupt
		}
		switch b.typ {
		case "ftyp", "styp", "free", "skip":
			if err := skipBox(rd.rs, b); err != nil {
				return err
			}
		case "moov":
			if err := rd.parseMoov(b); err != nil {
				return err
			}
		case "mdat":
			if _, err := rd.rs.Seek(off+b.size, io.SeekStart); err != nil {
				return err
			}
		case "moof":
			if err := rd.parseMoof(b, off); err != nil {
				return err
			}
		default:
			if err := skipBox(rd.rs, b); err != nil {
				return err
			}
		}
		_ = off
	}
	if len(rd.tracks) == 0 {
		return ErrCorrupt
	}
	rd.sortFragments()
	return nil
}

// parseMoov reads moov fully and walks its children (mvhd, trak).
func (rd *Reader) parseMoov(b box) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(rd.rs, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "mvhd":
			if err := rd.parseMvhd(mr, cb); err != nil {
				return err
			}
		case "trak":
			if err := rd.parseTrak(mr, cb); err != nil {
				return err
			}
		case "mvex":
			if err := rd.parseMvex(mr, cb); err != nil {
				return err
			}
		case "udta", "meta":
			if err := rd.parseMetadataContainer(mr, cb, cb.typ == "meta"); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseTrak reads a trak box fully and walks tkhd/mdia.
func (rd *Reader) parseTrak(r io.Reader, b box) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	tr := bytes.NewReader(buf)
	t := &trackState{info: Track{Number: len(rd.tracks) + 1}}
	for tr.Len() > 0 {
		cb, err := readBox(tr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "tkhd":
			if err := rd.parseTkhd(tr, cb, t); err != nil {
				return err
			}
		case "edts":
			if err := rd.parseEdts(tr, cb, t); err != nil {
				return err
			}
		case "mdia":
			if err := rd.parseMdia(tr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(tr, cb); err != nil {
				return err
			}
		}
	}
	if t.timescale == 0 {
		return nil // no mdhd; skip
	}
	t.info.Timescale = t.timescale
	t.info.Duration = t.duration
	if t.hasEditMediaTime && t.editMediaTime >= 0 {
		t.presentationShift = -t.editMediaTime
	}
	if rd.movieTimescale > 0 && t.editLeadMovie > 0 {
		lead, ok := rescaleUnsigned(t.editLeadMovie, rd.movieTimescale, t.timescale)
		if !ok || lead > math.MaxInt64 {
			return ErrCorrupt
		}
		shift, ok := checkedAddInt64(t.presentationShift, int64(lead))
		if !ok {
			return ErrCorrupt
		}
		t.presentationShift = shift
	}
	rd.tracks = append(rd.tracks, t)
	return nil
}

// parseMdia walks mdhd + minf.
func (rd *Reader) parseMdia(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "mdhd":
			if err := rd.parseMdhd(mr, cb, t); err != nil {
				return err
			}
		case "minf":
			if err := rd.parseMinf(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseMdhd reads the media timescale (4 bytes after version/flags + times).
func (rd *Reader) parseMdhd(r io.Reader, b box, t *trackState) error {
	// fullbox: version(1)+flags(3) = 4 bytes, then the mdhd body.
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	version := hdr[0]
	rest := int64(b.payload) - 4
	// creation + modification (8 bytes v0, 16 bytes v1).
	var skipN int64 = 8
	if version == 1 {
		skipN = 16
	}
	if _, err := io.CopyN(io.Discard, r, skipN); err != nil {
		return err
	}
	rest -= skipN
	var ts [4]byte
	if _, err := io.ReadFull(r, ts[:]); err != nil {
		return err
	}
	t.timescale = binary.BigEndian.Uint32(ts[:])
	rest -= 4
	// duration (8 bytes v1, 4 bytes v0).
	// body (language + predefined) is skipped from the correct offset.
	var dur int64 = 4
	if version == 1 {
		dur = 8
	}
	var durationBytes [8]byte
	if _, err := io.ReadFull(r, durationBytes[:dur]); err != nil {
		return err
	}
	if version == 1 {
		t.duration = binary.BigEndian.Uint64(durationBytes[:])
	} else {
		t.duration = uint64(binary.BigEndian.Uint32(durationBytes[:4]))
	}
	rest -= dur
	if rest >= 2 {
		var language [2]byte
		if _, err := io.ReadFull(r, language[:]); err != nil {
			return err
		}
		t.info.Language = decodeLanguage(binary.BigEndian.Uint16(language[:]))
		rest -= 2
	}
	if rest > 0 {
		if _, err := io.CopyN(io.Discard, r, rest); err != nil {
			return err
		}
	}
	return nil
}

// parseMinf walks straight to stbl.
func (rd *Reader) parseMinf(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "stbl":
			if err := rd.parseStbl(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseStbl walks the sample table boxes.
func (rd *Reader) parseStbl(r io.Reader, b box, t *trackState) error {
	if b.payload < 0 || b.payload > 1<<30 {
		return ErrCorrupt
	}
	buf := make([]byte, b.payload)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	mr := bytes.NewReader(buf)
	for mr.Len() > 0 {
		cb, err := readBox(mr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		switch cb.typ {
		case "stsd":
			if err := rd.parseStsd(mr, cb, t); err != nil {
				return err
			}
		case "stts":
			if err := rd.parseStts(mr, cb, t); err != nil {
				return err
			}
		case "ctts":
			if err := rd.parseCtts(mr, cb, t); err != nil {
				return err
			}
		case "stsz":
			if err := rd.parseStsz(mr, cb, t); err != nil {
				return err
			}
		case "stsc":
			if err := rd.parseStsc(mr, cb, t); err != nil {
				return err
			}
		case "stco":
			if err := rd.parseStco(mr, cb, t, false); err != nil {
				return err
			}
		case "co64":
			if err := rd.parseStco(mr, cb, t, true); err != nil {
				return err
			}
		case "stss":
			if err := rd.parseStss(mr, cb, t); err != nil {
				return err
			}
		default:
			if err := skipBox(mr, cb); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseStsd(r io.Reader, b box, t *trackState) error {
	if b.payload < 8 {
		return ErrCorrupt
	}
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	entryCount := binary.BigEndian.Uint32(hdr[4:8])
	remaining := int64(b.payload) - 8
	if entryCount == 0 || remaining < 8 {
		if remaining > 0 {
			if _, err := io.CopyN(io.Discard, r, remaining); err != nil {
				return err
			}
		}
		return nil
	}
	// First sample entry: size(4) + type(4) + entry body.
	var se [8]byte
	if _, err := io.ReadFull(r, se[:]); err != nil {
		return err
	}
	remaining -= 8
	entrySize := int64(binary.BigEndian.Uint32(se[0:4]))
	codecType := string(se[4:8])
	detected := codecFromSampleEntry(codecType, t.info.Number)
	t.info.Codec = detected.Codec
	t.info.IsVideo = detected.IsVideo
	if t.info.Channels == 0 {
		t.info.Channels = detected.Channels
	}
	if t.info.SampleRate == 0 {
		t.info.SampleRate = detected.SampleRate
	}
	// Read this sample entry's body (after size+type), bounded by the bytes
	// actually remaining in the stsd box.
	bodyLen := entrySize - 8
	if bodyLen < 0 || bodyLen > remaining {
		bodyLen = remaining
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	remaining -= bodyLen
	if t.info.IsVideo && len(body) >= 28 {
		// VisualSampleEntry layout after size+type: reserved(6) +
		// data_reference_index(2) [8], then pre_defined(2) + reserved(2) +
		// pre_defined[3](12) [16], then width(2) + height(2) at body offset 24.
		// Previously width/height were never parsed, so remuxed video tracks
		// carried PixelWidth/PixelHeight = 0.
		t.info.Width = int(binary.BigEndian.Uint16(body[24:26]))
		t.info.Height = int(binary.BigEndian.Uint16(body[26:28]))
	}
	childOffset := 0
	if t.info.IsVideo && len(body) >= 78 {
		childOffset = 78
	} else if !t.info.IsVideo && len(body) >= 28 {
		t.info.Channels = int(binary.BigEndian.Uint16(body[16:18]))
		t.info.SampleRate = float64(binary.BigEndian.Uint32(body[24:28])) / 65536
		childOffset = 28
	}
	if childOffset > 0 && childOffset < len(body) {
		if err := parseSampleEntryChildren(body[childOffset:], t); err != nil {
			return err
		}
	}
	if remaining > 0 {
		if _, err := io.CopyN(io.Discard, r, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (rd *Reader) parseCtts(r io.Reader, b box, t *trackState) error {
	if b.payload < 8 {
		return ErrCorrupt
	}
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	version := header[0]
	count := binary.BigEndian.Uint32(header[4:8])
	if uint64(count) > uint64(b.payload-8)/8 {
		return ErrCorrupt
	}
	for range count {
		var row [8]byte
		if _, err := io.ReadFull(r, row[:]); err != nil {
			return err
		}
		offset := int64(binary.BigEndian.Uint32(row[4:8]))
		if version == 1 {
			offset = int64(int32(binary.BigEndian.Uint32(row[4:8])))
		} else if version != 0 {
			return ErrCorrupt
		}
		t.ctts = append(t.ctts, cttsEntry{count: binary.BigEndian.Uint32(row[0:4]), offset: offset})
	}
	return nil
}

func parseSampleEntryChildren(data []byte, t *trackState) error {
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		b, err := readBox(r)
		if err != nil {
			return err
		}
		if b.payload < 0 || b.payload > int64(r.Len()) {
			return ErrCorrupt
		}
		payload := make([]byte, b.payload)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		switch b.typ {
		case "avcC", "hvcC", "av1C", "vpcC", "dOps", "dfLa":
			t.info.CodecConfigType = b.typ
			t.info.CodecConfig = payload
		case "esds":
			if config := findESDSDecoderConfig(payload); len(config) > 0 {
				t.info.CodecConfigType = "asc"
				t.info.CodecConfig = config
			}
		}
	}
	return nil
}

func findESDSDecoderConfig(data []byte) []byte {
	if len(data) < 4 {
		return nil
	}
	for i := 4; i < len(data); i++ {
		if data[i] != 0x05 {
			continue
		}
		length, used, ok := readDescriptorLength(data[i+1:])
		start := i + 1 + used
		if ok && length > 0 && start <= len(data) && length <= len(data)-start {
			return append([]byte(nil), data[start:start+length]...)
		}
	}
	return nil
}

func readDescriptorLength(data []byte) (int, int, bool) {
	length := 0
	for i := 0; i < len(data) && i < 4; i++ {
		if length > (1 << 24) {
			return 0, 0, false
		}
		length = length<<7 | int(data[i]&0x7f)
		if data[i]&0x80 == 0 {
			return length, i + 1, true
		}
	}
	return 0, 0, false
}

func (rd *Reader) parseStts(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stts = append(t.stts, sttsEntry{
			count: binary.BigEndian.Uint32(e[0:4]),
			delta: binary.BigEndian.Uint32(e[4:8]),
		})
	}
	return nil
}

func (rd *Reader) parseStsz(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	t.uniform = binary.BigEndian.Uint32(hdr[4:8])
	count := binary.BigEndian.Uint32(hdr[8:12])
	if t.uniform != 0 {
		// Uniform sizes; per-sample table omitted. Skip any leftover.
		leftover := int64(b.payload) - 12
		if leftover > 0 {
			if _, err := io.CopyN(io.Discard, r, leftover); err != nil {
				return err
			}
		}
		return nil
	}
	for i := uint32(0); i < count; i++ {
		var e [4]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.sampleSize = append(t.sampleSize, binary.BigEndian.Uint32(e[:]))
	}
	return nil
}

func (rd *Reader) parseStsc(r io.Reader, b box, t *trackState) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [12]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stsc = append(t.stsc, stscEntry{
			firstChunk:      binary.BigEndian.Uint32(e[0:4]),
			samplesPerChunk: binary.BigEndian.Uint32(e[4:8]),
			sampleDescIndex: binary.BigEndian.Uint32(e[8:12]),
		})
	}
	return nil
}

func (rd *Reader) parseStco(r io.Reader, b box, t *trackState, co64 bool) error {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		if co64 {
			var e [8]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				return err
			}
			t.stco = append(t.stco, binary.BigEndian.Uint64(e[:]))
		} else {
			var e [4]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				return err
			}
			t.stco = append(t.stco, uint64(binary.BigEndian.Uint32(e[:])))
		}
	}
	return nil
}

func (rd *Reader) parseStss(r io.Reader, b box, t *trackState) error {
	t.hasSTSS = true
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(hdr[4:8])
	for i := uint32(0); i < count; i++ {
		var e [4]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
		t.stss = append(t.stss, binary.BigEndian.Uint32(e[:]))
	}
	return nil
}
