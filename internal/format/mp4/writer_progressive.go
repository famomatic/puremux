package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/famomatic/puremux/internal/core"
)

const movieTimeScale uint32 = 1_000_000_000

type progressiveSample struct {
	dts      int64
	pts      int64
	duration uint32
	offset   uint64
	size     uint32
	keyframe bool
}

type progressiveTrack struct {
	spec    OutputTrack
	samples []progressiveSample
}

// ProgressiveWriter writes a seekable, moov-at-end MP4 without retaining
// compressed payloads. Only O(number of samples) table metadata is kept.
type ProgressiveWriter struct {
	w           io.WriteSeeker
	tracks      []*progressiveTrack
	trackByID   map[int]*progressiveTrack
	started     bool
	closed      bool
	closeErr    error
	mdatStart   int64
	mdatPayload uint64
}

func NewProgressiveWriter(w io.WriteSeeker) (*ProgressiveWriter, error) {
	if w == nil {
		return nil, errors.New("mp4: nil progressive writer")
	}
	return &ProgressiveWriter{w: w, trackByID: make(map[int]*progressiveTrack)}, nil
}

func (w *ProgressiveWriter) AddTrack(spec OutputTrack) (int, error) {
	if w.closed || w.started {
		return 0, errors.New("mp4: cannot add track after writing starts")
	}
	if spec.ID == 0 {
		spec.ID = len(w.tracks) + 1
	}
	if err := validateOutputTrack(spec); err != nil {
		return 0, err
	}
	if _, exists := w.trackByID[spec.ID]; exists {
		return 0, ErrInvalidOutputTrack
	}
	t := &progressiveTrack{spec: spec}
	w.tracks = append(w.tracks, t)
	w.trackByID[spec.ID] = t
	return spec.ID, nil
}

func (w *ProgressiveWriter) start() error {
	if w.started {
		return nil
	}
	ftyp, err := makeFileTypeBox(false, progressiveHasCodec(w.tracks, core.CodecAV1))
	if err != nil {
		return err
	}
	if err := writeFull(w.w, ftyp); err != nil {
		return err
	}
	start, err := w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.mdatStart = start
	// Always use the 64-bit large-size header so mdat growth never shifts data.
	var header [16]byte
	binary.BigEndian.PutUint32(header[0:4], 1)
	copy(header[4:8], "mdat")
	binary.BigEndian.PutUint64(header[8:16], 16)
	if err := writeFull(w.w, header[:]); err != nil {
		return err
	}
	w.started = true
	return nil
}

func (w *ProgressiveWriter) WriteSample(sample OutputSample) error {
	if w.closed {
		return io.ErrClosedPipe
	}
	t := w.trackByID[sample.TrackID]
	if t == nil || sample.Duration <= 0 || sample.Duration > math.MaxUint32 ||
		uint64(len(sample.Data)) > uint64(math.MaxUint32) {
		return ErrInvalidOutputSample
	}
	composition, valid := checkedSubInt64(sample.PTS, sample.DTS)
	_, validEnd := checkedAddInt64(sample.DTS, sample.Duration)
	if !valid || !validEnd || composition < math.MinInt32 || composition > math.MaxInt32 {
		return ErrInvalidOutputSample
	}
	if n := len(t.samples); n > 0 {
		prev := t.samples[n-1]
		prevEnd, ok := checkedAddInt64(prev.dts, int64(prev.duration))
		if !ok || sample.DTS != prevEnd {
			return ErrInvalidOutputSample
		}
	}
	if uint64(len(sample.Data)) > math.MaxUint64-w.mdatPayload {
		return ErrOutputTooLarge
	}
	if err := w.start(); err != nil {
		return err
	}
	off, err := w.w.Seek(0, io.SeekCurrent)
	if err != nil || off < 0 {
		return ErrOutputTooLarge
	}
	if err := writeFull(w.w, sample.Data); err != nil {
		return err
	}
	w.mdatPayload += uint64(len(sample.Data))
	t.samples = append(t.samples, progressiveSample{
		dts: sample.DTS, pts: sample.PTS, duration: uint32(sample.Duration),
		offset: uint64(off), size: uint32(len(sample.Data)), keyframe: sample.Keyframe,
	})
	return nil
}

func (w *ProgressiveWriter) Close() (retErr error) {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	defer func() { w.closeErr = retErr }()
	if len(w.tracks) == 0 {
		return ErrInvalidOutputTrack
	}
	if err := w.start(); err != nil {
		return err
	}
	end, err := w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if w.mdatPayload > math.MaxUint64-16 {
		return ErrOutputTooLarge
	}
	if _, err = w.w.Seek(w.mdatStart+8, io.SeekStart); err != nil {
		return err
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], w.mdatPayload+16)
	if err = writeFull(w.w, size[:]); err != nil {
		return err
	}
	if _, err = w.w.Seek(end, io.SeekStart); err != nil {
		return err
	}
	moov, err := makeProgressiveMoov(w.tracks)
	if err != nil {
		return err
	}
	return writeFull(w.w, moov)
}

func makeFileTypeBox(fragmented, hasAV1 bool) ([]byte, error) {
	var p bytes.Buffer
	p.WriteString("isom")
	putU32(&p, 0x200)
	p.WriteString("isom")
	p.WriteString("iso6")
	if hasAV1 {
		p.WriteString("av01")
	}
	if fragmented {
		p.WriteString("dash")
	} else {
		p.WriteString("mp41")
	}
	return outputBox("ftyp", p.Bytes())
}

func progressiveHasCodec(tracks []*progressiveTrack, codec core.CodecType) bool {
	for _, track := range tracks {
		if track.spec.Codec == codec {
			return true
		}
	}
	return false
}

func makeProgressiveMoov(tracks []*progressiveTrack) ([]byte, error) {
	globalStartSet := false
	var globalStart int64
	var globalScale uint32
	for _, track := range tracks {
		if len(track.samples) == 0 {
			continue
		}
		first := track.samples[0].dts
		if !globalStartSet || timestampLess(first, track.spec.TimeScale, globalStart, globalScale) {
			globalStart, globalScale, globalStartSet = first, track.spec.TimeScale, true
		}
	}
	var payload bytes.Buffer
	var movieDuration uint64
	for _, track := range tracks {
		delay := uint64(0)
		if len(track.samples) > 0 && globalStartSet {
			firstMovie, ok1 := rescaleSigned(track.samples[0].dts, track.spec.TimeScale, movieTimeScale)
			globalMovie, ok2 := rescaleSigned(globalStart, globalScale, movieTimeScale)
			if !ok1 || !ok2 || firstMovie < globalMovie {
				return nil, ErrOutputTooLarge
			}
			delay = uint64(firstMovie) - uint64(globalMovie)
		}
		presentationDuration, err := trackPresentationDuration(track)
		if err != nil {
			return nil, err
		}
		movieTrackDuration, ok := rescaleUnsigned(presentationDuration, track.spec.TimeScale, movieTimeScale)
		if !ok || movieTrackDuration > math.MaxUint64-delay {
			return nil, ErrOutputTooLarge
		}
		movieTrackDuration += delay
		if movieTrackDuration > movieDuration {
			movieDuration = movieTrackDuration
		}
	}
	mvhd, err := makeMVHD(movieDuration, uint32(len(tracks)+1), true)
	if err != nil {
		return nil, err
	}
	payload.Write(mvhd)
	for _, track := range tracks {
		trak, err := makeProgressiveTrak(track, globalStart, globalScale, globalStartSet)
		if err != nil {
			return nil, err
		}
		payload.Write(trak)
	}
	return outputBox("moov", payload.Bytes())
}

func trackPresentationDuration(t *progressiveTrack) (uint64, error) {
	if len(t.samples) == 0 {
		return 0, nil
	}
	first := t.samples[0].dts
	end := first
	for _, sample := range t.samples {
		candidate, ok := checkedAddInt64(sample.pts, int64(sample.duration))
		if !ok {
			return 0, ErrInvalidOutputSample
		}
		if candidate > end {
			end = candidate
		}
		decodeEnd, ok := checkedAddInt64(sample.dts, int64(sample.duration))
		if !ok {
			return 0, ErrInvalidOutputSample
		}
		if decodeEnd > end {
			end = decodeEnd
		}
	}
	if end < first {
		return 0, ErrInvalidOutputSample
	}
	return uint64(end) - uint64(first), nil
}

func trackDecodeDuration(t *progressiveTrack) (uint64, error) {
	if len(t.samples) == 0 {
		return 0, nil
	}
	first := t.samples[0].dts
	last := t.samples[len(t.samples)-1]
	end, ok := checkedAddInt64(last.dts, int64(last.duration))
	if !ok || end < first {
		return 0, ErrInvalidOutputSample
	}
	return uint64(end) - uint64(first), nil
}

func makeProgressiveTrak(t *progressiveTrack, globalStart int64, globalScale uint32, haveGlobal bool) ([]byte, error) {
	presentationDuration, err := trackPresentationDuration(t)
	if err != nil {
		return nil, err
	}
	delay := uint64(0)
	if len(t.samples) > 0 && haveGlobal {
		firstMovie, ok1 := rescaleSigned(t.samples[0].dts, t.spec.TimeScale, movieTimeScale)
		globalMovie, ok2 := rescaleSigned(globalStart, globalScale, movieTimeScale)
		if !ok1 || !ok2 || firstMovie < globalMovie {
			return nil, ErrOutputTooLarge
		}
		delay = uint64(firstMovie) - uint64(globalMovie)
	}
	movieDuration, ok := rescaleUnsigned(presentationDuration, t.spec.TimeScale, movieTimeScale)
	if !ok || movieDuration > math.MaxUint64-delay {
		return nil, ErrOutputTooLarge
	}
	movieDuration += delay
	var payload bytes.Buffer
	tkhd, err := makeTKHD(t.spec, movieDuration, true)
	if err != nil {
		return nil, err
	}
	payload.Write(tkhd)
	if delay > 0 {
		edts, err := makeEDTS(delay, movieDuration-delay)
		if err != nil {
			return nil, err
		}
		payload.Write(edts)
	}
	decodeDuration, err := trackDecodeDuration(t)
	if err != nil {
		return nil, err
	}
	mdia, err := makeProgressiveMDIA(t, decodeDuration)
	if err != nil {
		return nil, err
	}
	payload.Write(mdia)
	return outputBox("trak", payload.Bytes())
}

func makeMVHD(duration uint64, nextTrackID uint32, version1 bool) ([]byte, error) {
	var p bytes.Buffer
	if version1 {
		putU64(&p, 0)
		putU64(&p, 0)
		putU32(&p, movieTimeScale)
		putU64(&p, duration)
	} else {
		putU32(&p, 0)
		putU32(&p, 0)
		putU32(&p, movieTimeScale)
		putU32(&p, uint32(duration))
	}
	putU32(&p, 0x00010000)
	putU16(&p, 0x0100)
	p.Write(make([]byte, 10))
	writeUnityMatrix(&p)
	p.Write(make([]byte, 24))
	putU32(&p, nextTrackID)
	version := byte(0)
	if version1 {
		version = 1
	}
	return outputFullBox("mvhd", version, 0, p.Bytes())
}

func makeTKHD(t OutputTrack, duration uint64, version1 bool) ([]byte, error) {
	var p bytes.Buffer
	if version1 {
		putU64(&p, 0)
		putU64(&p, 0)
		putU32(&p, uint32(t.ID))
		putU32(&p, 0)
		putU64(&p, duration)
	} else {
		putU32(&p, 0)
		putU32(&p, 0)
		putU32(&p, uint32(t.ID))
		putU32(&p, 0)
		putU32(&p, uint32(duration))
	}
	p.Write(make([]byte, 8))
	putU16(&p, 0)
	putU16(&p, 0)
	if t.Codec.IsAudio() {
		putU16(&p, 0x0100)
	} else {
		putU16(&p, 0)
	}
	putU16(&p, 0)
	writeUnityMatrix(&p)
	putU32(&p, uint32(t.Width)<<16)
	putU32(&p, uint32(t.Height)<<16)
	version := byte(0)
	if version1 {
		version = 1
	}
	return outputFullBox("tkhd", version, 0x000007, p.Bytes())
}

func writeUnityMatrix(p *bytes.Buffer) {
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		putU32(p, v)
	}
}

func makeEDTS(delay, mediaDuration uint64) ([]byte, error) {
	var p bytes.Buffer
	putU32(&p, 2)
	putU64(&p, delay)
	putI64(&p, -1)
	putU16(&p, 1)
	putU16(&p, 0)
	putU64(&p, mediaDuration)
	putI64(&p, 0)
	putU16(&p, 1)
	putU16(&p, 0)
	elst, err := outputFullBox("elst", 1, 0, p.Bytes())
	if err != nil {
		return nil, err
	}
	return outputBox("edts", elst)
}

func makeProgressiveMDIA(t *progressiveTrack, duration uint64) ([]byte, error) {
	var p bytes.Buffer
	mdhd, err := makeMDHDLanguage(t.spec.TimeScale, duration, t.spec.Language)
	if err != nil {
		return nil, err
	}
	p.Write(mdhd)
	hdlr, err := makeHDLR(t.spec.Codec.IsVideo())
	if err != nil {
		return nil, err
	}
	p.Write(hdlr)
	minf, err := makeProgressiveMINF(t)
	if err != nil {
		return nil, err
	}
	p.Write(minf)
	return outputBox("mdia", p.Bytes())
}

func makeMDHD(scale uint32, duration uint64) ([]byte, error) {
	return makeMDHDLanguage(scale, duration, "und")
}

func makeMDHDLanguage(scale uint32, duration uint64, language string) ([]byte, error) {
	if language == "" {
		language = "und"
	}
	if len(language) != 3 {
		return nil, ErrInvalidOutputTrack
	}
	for _, c := range language {
		if c < 'a' || c > 'z' {
			return nil, ErrInvalidOutputTrack
		}
	}
	var p bytes.Buffer
	putU64(&p, 0)
	putU64(&p, 0)
	putU32(&p, scale)
	putU64(&p, duration)
	// ISO-639-2/T "und": 21,14,4 packed into 5-bit fields, MSB-first.
	putU16(&p, uint16(language[0]-0x60)<<10|uint16(language[1]-0x60)<<5|uint16(language[2]-0x60))
	putU16(&p, 0)
	return outputFullBox("mdhd", 1, 0, p.Bytes())
}

func makeHDLR(video bool) ([]byte, error) {
	var p bytes.Buffer
	putU32(&p, 0)
	if video {
		p.WriteString("vide")
	} else {
		p.WriteString("soun")
	}
	p.Write(make([]byte, 12))
	if video {
		p.WriteString("VideoHandler\x00")
	} else {
		p.WriteString("SoundHandler\x00")
	}
	return outputFullBox("hdlr", 0, 0, p.Bytes())
}

func makeDINF() ([]byte, error) {
	url, err := outputFullBox("url ", 0, 1, nil)
	if err != nil {
		return nil, err
	}
	var p bytes.Buffer
	putU32(&p, 1)
	p.Write(url)
	dref, err := outputFullBox("dref", 0, 0, p.Bytes())
	if err != nil {
		return nil, err
	}
	return outputBox("dinf", dref)
}

func makeProgressiveMINF(t *progressiveTrack) ([]byte, error) {
	var p bytes.Buffer
	if t.spec.Codec.IsVideo() {
		vmhd, _ := outputFullBox("vmhd", 0, 1, make([]byte, 8))
		p.Write(vmhd)
	} else {
		smhd, _ := outputFullBox("smhd", 0, 0, make([]byte, 4))
		p.Write(smhd)
	}
	dinf, err := makeDINF()
	if err != nil {
		return nil, err
	}
	p.Write(dinf)
	stbl, err := makeProgressiveSTBL(t)
	if err != nil {
		return nil, err
	}
	p.Write(stbl)
	return outputBox("minf", p.Bytes())
}

func makeProgressiveSTBL(t *progressiveTrack) ([]byte, error) {
	var p bytes.Buffer
	entry, err := sampleEntry(t.spec)
	if err != nil {
		return nil, err
	}
	var stsdP bytes.Buffer
	putU32(&stsdP, 1)
	stsdP.Write(entry)
	stsd, _ := outputFullBox("stsd", 0, 0, stsdP.Bytes())
	p.Write(stsd)
	stts, err := makeSTTS(t.samples)
	if err != nil {
		return nil, err
	}
	p.Write(stts)
	ctts, err := makeCTTS(t.samples)
	if err != nil {
		return nil, err
	}
	p.Write(ctts)
	stss, err := makeSTSS(t)
	if err != nil {
		return nil, err
	}
	p.Write(stss)
	stsc, _ := makeSingleSampleSTSC(len(t.samples))
	p.Write(stsc)
	stsz, err := makeSTSZ(t.samples)
	if err != nil {
		return nil, err
	}
	p.Write(stsz)
	stco, err := makeChunkOffsets(t.samples)
	if err != nil {
		return nil, err
	}
	p.Write(stco)
	return outputBox("stbl", p.Bytes())
}

type timeRun struct{ count, value uint32 }

func makeSTTS(samples []progressiveSample) ([]byte, error) {
	runs := make([]timeRun, 0)
	for _, s := range samples {
		if len(runs) > 0 && runs[len(runs)-1].value == s.duration {
			runs[len(runs)-1].count++
		} else {
			runs = append(runs, timeRun{1, s.duration})
		}
	}
	var p bytes.Buffer
	putU32(&p, uint32(len(runs)))
	for _, r := range runs {
		putU32(&p, r.count)
		putU32(&p, r.value)
	}
	return outputFullBox("stts", 0, 0, p.Bytes())
}

type compositionRun struct {
	count  uint32
	offset int32
}

func makeCTTS(samples []progressiveSample) ([]byte, error) {
	needed := false
	runs := make([]compositionRun, 0)
	for _, s := range samples {
		off64, ok := checkedSubInt64(s.pts, s.dts)
		if !ok || off64 < math.MinInt32 || off64 > math.MaxInt32 {
			return nil, ErrInvalidOutputSample
		}
		off := int32(off64)
		needed = needed || off != 0
		if len(runs) > 0 && runs[len(runs)-1].offset == off {
			runs[len(runs)-1].count++
		} else {
			runs = append(runs, compositionRun{1, off})
		}
	}
	if !needed {
		return nil, nil
	}
	var p bytes.Buffer
	putU32(&p, uint32(len(runs)))
	for _, r := range runs {
		putU32(&p, r.count)
		putI32(&p, r.offset)
	}
	return outputFullBox("ctts", 1, 0, p.Bytes())
}

func makeSTSS(t *progressiveTrack) ([]byte, error) {
	if !t.spec.Codec.IsVideo() {
		return nil, nil
	}
	allSync := true
	for _, s := range t.samples {
		if !s.keyframe {
			allSync = false
			break
		}
	}
	if allSync {
		return nil, nil
	}
	var p bytes.Buffer
	count := uint32(0)
	putU32(&p, 0)
	for i, s := range t.samples {
		if s.keyframe {
			putU32(&p, uint32(i+1))
			count++
		}
	}
	binary.BigEndian.PutUint32(p.Bytes()[0:4], count)
	return outputFullBox("stss", 0, 0, p.Bytes())
}

func makeSingleSampleSTSC(sampleCount int) ([]byte, error) {
	var p bytes.Buffer
	if sampleCount == 0 {
		putU32(&p, 0)
	} else {
		putU32(&p, 1)
		putU32(&p, 1)
		putU32(&p, 1)
		putU32(&p, 1)
	}
	return outputFullBox("stsc", 0, 0, p.Bytes())
}

func makeSTSZ(samples []progressiveSample) ([]byte, error) {
	var p bytes.Buffer
	putU32(&p, 0)
	putU32(&p, uint32(len(samples)))
	for _, s := range samples {
		putU32(&p, s.size)
	}
	return outputFullBox("stsz", 0, 0, p.Bytes())
}

func makeChunkOffsets(samples []progressiveSample) ([]byte, error) {
	large := false
	for _, s := range samples {
		if s.offset > math.MaxUint32 {
			large = true
			break
		}
	}
	var p bytes.Buffer
	putU32(&p, uint32(len(samples)))
	if large {
		for _, s := range samples {
			putU64(&p, s.offset)
		}
		return outputFullBox("co64", 0, 0, p.Bytes())
	}
	for _, s := range samples {
		putU32(&p, uint32(s.offset))
	}
	return outputFullBox("stco", 0, 0, p.Bytes())
}

func rescaleUnsigned(value uint64, from, to uint32) (uint64, bool) {
	if from == 0 {
		return 0, false
	}
	whole, rem := value/uint64(from), value%uint64(from)
	if whole > math.MaxUint64/uint64(to) {
		return 0, false
	}
	return whole*uint64(to) + rem*uint64(to)/uint64(from), true
}

func rescaleSigned(value int64, from, to uint32) (int64, bool) {
	negative := value < 0
	var magnitude uint64
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	} else {
		magnitude = uint64(value)
	}
	out, ok := rescaleUnsigned(magnitude, from, to)
	if !ok || (!negative && out > math.MaxInt64) || (negative && out > uint64(1)<<63) {
		return 0, false
	}
	if negative {
		if out == uint64(1)<<63 {
			return math.MinInt64, true
		}
		return -int64(out), true
	}
	return int64(out), true
}
