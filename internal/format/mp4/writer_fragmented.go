package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

const (
	defaultFragmentDuration = 2 * time.Second
	defaultMaxFragmentBytes = 32 << 20
)

var fragmentBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

type bufferedFragmentSample struct {
	sample OutputSample
	offset int
	size   int
}

type fragmentedTrack struct {
	spec       OutputTrack
	lastEnd    int64
	haveSample bool
}

// FragmentedWriter writes an initialization segment followed by bounded
// moof+mdat fragments to any io.Writer.
type FragmentedWriter struct {
	w                io.Writer
	tracks           []*fragmentedTrack
	trackByID        map[int]*fragmentedTrack
	pending          []bufferedFragmentSample
	payload          *bytes.Buffer
	fragmentDuration time.Duration
	maxFragmentBytes int
	sequence         uint32
	started          bool
	closed           bool
	closeErr         error
}

func NewFragmentedWriter(w io.Writer, fragmentDuration time.Duration, maxFragmentBytes int) (*FragmentedWriter, error) {
	if w == nil || fragmentDuration < 0 || maxFragmentBytes < 0 {
		return nil, errors.New("mp4: invalid fragmented writer configuration")
	}
	if fragmentDuration == 0 {
		fragmentDuration = defaultFragmentDuration
	}
	if maxFragmentBytes == 0 {
		maxFragmentBytes = defaultMaxFragmentBytes
	}
	b := fragmentBufferPool.Get().(*bytes.Buffer)
	b.Reset()
	return &FragmentedWriter{
		w: w, trackByID: make(map[int]*fragmentedTrack), payload: b,
		fragmentDuration: fragmentDuration, maxFragmentBytes: maxFragmentBytes, sequence: 1,
	}, nil
}

func (w *FragmentedWriter) AddTrack(spec OutputTrack) (int, error) {
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
	t := &fragmentedTrack{spec: spec}
	w.tracks = append(w.tracks, t)
	w.trackByID[spec.ID] = t
	return spec.ID, nil
}

func (w *FragmentedWriter) start() error {
	if w.started {
		return nil
	}
	if len(w.tracks) == 0 {
		return ErrInvalidOutputTrack
	}
	hasAV1 := false
	for _, track := range w.tracks {
		if track.spec.Codec == core.CodecAV1 {
			hasAV1 = true
			break
		}
	}
	ftyp, err := makeFileTypeBox(true, hasAV1)
	if err != nil {
		return err
	}
	moov, err := makeFragmentedMoov(w.tracks)
	if err != nil {
		return err
	}
	if err := writeFull(w.w, ftyp); err != nil {
		return err
	}
	if err := writeFull(w.w, moov); err != nil {
		return err
	}
	w.started = true
	return nil
}

func (w *FragmentedWriter) WriteSample(sample OutputSample) error {
	if w.closed {
		return io.ErrClosedPipe
	}
	t := w.trackByID[sample.TrackID]
	if t == nil || sample.DTS < 0 || sample.Duration <= 0 || sample.Duration > math.MaxUint32 ||
		len(sample.Data) == 0 || uint64(len(sample.Data)) > uint64(math.MaxUint32) || len(sample.Data) > w.maxFragmentBytes {
		return ErrInvalidOutputSample
	}
	composition, valid := checkedSubInt64(sample.PTS, sample.DTS)
	end, validEnd := checkedAddInt64(sample.DTS, sample.Duration)
	if !valid || !validEnd || composition < math.MinInt32 || composition > math.MaxInt32 {
		return ErrInvalidOutputSample
	}
	if t.haveSample && sample.DTS != t.lastEnd {
		return ErrInvalidOutputSample
	}
	if w.shouldFlushBefore(sample) {
		if err := w.flush(); err != nil {
			return err
		}
	}
	if w.payload.Len() > w.maxFragmentBytes-len(sample.Data) && len(w.pending) > 0 {
		if err := w.flush(); err != nil {
			return err
		}
	}
	if err := w.start(); err != nil {
		return err
	}
	off := w.payload.Len()
	_, _ = w.payload.Write(sample.Data)
	copySample := sample
	copySample.Data = nil
	w.pending = append(w.pending, bufferedFragmentSample{sample: copySample, offset: off, size: len(sample.Data)})
	t.lastEnd = end
	t.haveSample = true
	if !w.hasVideo() && w.pendingDuration() >= w.fragmentDuration {
		return w.flush()
	}
	return nil
}

func (w *FragmentedWriter) shouldFlushBefore(next OutputSample) bool {
	if len(w.pending) == 0 || !next.Keyframe || !w.trackByID[next.TrackID].spec.Codec.IsVideo() {
		return false
	}
	for _, sample := range w.pending {
		if sample.sample.TrackID == next.TrackID {
			return true
		}
	}
	return false
}

func (w *FragmentedWriter) hasVideo() bool {
	for _, t := range w.tracks {
		if t.spec.Codec.IsVideo() {
			return true
		}
	}
	return false
}

func (w *FragmentedWriter) pendingDuration() time.Duration {
	if len(w.pending) == 0 {
		return 0
	}
	first, last := w.pending[0], w.pending[0]
	for _, sample := range w.pending[1:] {
		if timestampLess(sample.sample.DTS, w.trackByID[sample.sample.TrackID].spec.TimeScale,
			first.sample.DTS, w.trackByID[first.sample.TrackID].spec.TimeScale) {
			first = sample
		}
		leftEnd, leftOK := checkedAddInt64(sample.sample.DTS, sample.sample.Duration)
		rightEnd, rightOK := checkedAddInt64(last.sample.DTS, last.sample.Duration)
		if !leftOK || !rightOK {
			return 0
		}
		if timestampLess(rightEnd, w.trackByID[last.sample.TrackID].spec.TimeScale,
			leftEnd, w.trackByID[sample.sample.TrackID].spec.TimeScale) {
			last = sample
		}
	}
	firstNS, ok1 := rescaleSigned(first.sample.DTS, w.trackByID[first.sample.TrackID].spec.TimeScale, 1_000_000_000)
	lastEnd, okEnd := checkedAddInt64(last.sample.DTS, last.sample.Duration)
	if !okEnd {
		return 0
	}
	lastNS, ok2 := rescaleSigned(lastEnd, w.trackByID[last.sample.TrackID].spec.TimeScale, 1_000_000_000)
	if !ok1 || !ok2 || lastNS <= firstNS {
		return 0
	}
	return time.Duration(lastNS - firstNS)
}

func (w *FragmentedWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	if err := w.start(); err != nil {
		return err
	}
	grouped := make(map[int][]bufferedFragmentSample, len(w.tracks))
	for _, sample := range w.pending {
		grouped[sample.sample.TrackID] = append(grouped[sample.sample.TrackID], sample)
	}
	trackOffsets := make(map[int]int, len(grouped))
	mdatPayload := fragmentBufferPool.Get().(*bytes.Buffer)
	mdatPayload.Reset()
	defer func() { mdatPayload.Reset(); fragmentBufferPool.Put(mdatPayload) }()
	data := w.payload.Bytes()
	for _, track := range w.tracks {
		trackOffsets[track.spec.ID] = mdatPayload.Len()
		for _, sample := range grouped[track.spec.ID] {
			_, _ = mdatPayload.Write(data[sample.offset : sample.offset+sample.size])
		}
	}
	moof, err := makeMOOF(w.sequence, w.tracks, grouped, trackOffsets, 0)
	if err != nil {
		return err
	}
	moof, err = makeMOOF(w.sequence, w.tracks, grouped, trackOffsets, len(moof)+8)
	if err != nil {
		return err
	}
	if err := writeFull(w.w, moof); err != nil {
		return err
	}
	mdat, err := outputBox("mdat", mdatPayload.Bytes())
	if err != nil {
		return err
	}
	if err := writeFull(w.w, mdat); err != nil {
		return err
	}
	w.sequence++
	w.pending = w.pending[:0]
	w.payload.Reset()
	return nil
}

func (w *FragmentedWriter) Close() (retErr error) {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	defer func() {
		w.closeErr = retErr
		if w.payload != nil {
			w.payload.Reset()
			fragmentBufferPool.Put(w.payload)
			w.payload = nil
		}
	}()
	if err := w.start(); err != nil {
		return err
	}
	return w.flush()
}

func makeFragmentedMoov(tracks []*fragmentedTrack) ([]byte, error) {
	var p bytes.Buffer
	mvhd, err := makeMVHD(0, uint32(len(tracks)+1), true)
	if err != nil {
		return nil, err
	}
	p.Write(mvhd)
	for _, t := range tracks {
		trak, err := makeFragmentedTrak(t.spec)
		if err != nil {
			return nil, err
		}
		p.Write(trak)
	}
	var mvexP bytes.Buffer
	for _, t := range tracks {
		var trexP bytes.Buffer
		putU32(&trexP, uint32(t.spec.ID))
		putU32(&trexP, 1)
		putU32(&trexP, 0)
		putU32(&trexP, 0)
		putU32(&trexP, 0)
		trex, _ := outputFullBox("trex", 0, 0, trexP.Bytes())
		mvexP.Write(trex)
	}
	mvex, err := outputBox("mvex", mvexP.Bytes())
	if err != nil {
		return nil, err
	}
	p.Write(mvex)
	return outputBox("moov", p.Bytes())
}

func makeFragmentedTrak(t OutputTrack) ([]byte, error) {
	var p bytes.Buffer
	tkhd, err := makeTKHD(t, 0, true)
	if err != nil {
		return nil, err
	}
	p.Write(tkhd)
	var mdiaP bytes.Buffer
	mdhd, err := makeMDHD(t.TimeScale, 0)
	if err != nil {
		return nil, err
	}
	mdiaP.Write(mdhd)
	hdlr, err := makeHDLR(t.Codec.IsVideo())
	if err != nil {
		return nil, err
	}
	mdiaP.Write(hdlr)
	minf, err := makeFragmentedMINF(t)
	if err != nil {
		return nil, err
	}
	mdiaP.Write(minf)
	mdia, err := outputBox("mdia", mdiaP.Bytes())
	if err != nil {
		return nil, err
	}
	p.Write(mdia)
	return outputBox("trak", p.Bytes())
}

func makeFragmentedMINF(t OutputTrack) ([]byte, error) {
	var p bytes.Buffer
	if t.Codec.IsVideo() {
		b, _ := outputFullBox("vmhd", 0, 1, make([]byte, 8))
		p.Write(b)
	} else {
		b, _ := outputFullBox("smhd", 0, 0, make([]byte, 4))
		p.Write(b)
	}
	dinf, err := makeDINF()
	if err != nil {
		return nil, err
	}
	p.Write(dinf)
	var stblP bytes.Buffer
	entry, err := sampleEntry(t)
	if err != nil {
		return nil, err
	}
	var stsdP bytes.Buffer
	putU32(&stsdP, 1)
	stsdP.Write(entry)
	stsd, _ := outputFullBox("stsd", 0, 0, stsdP.Bytes())
	stblP.Write(stsd)
	for _, typ := range []string{"stts", "stsc"} {
		b, _ := outputFullBox(typ, 0, 0, []byte{0, 0, 0, 0})
		stblP.Write(b)
	}
	b, _ := outputFullBox("stsz", 0, 0, make([]byte, 8))
	stblP.Write(b)
	b, _ = outputFullBox("stco", 0, 0, []byte{0, 0, 0, 0})
	stblP.Write(b)
	stbl, err := outputBox("stbl", stblP.Bytes())
	if err != nil {
		return nil, err
	}
	p.Write(stbl)
	return outputBox("minf", p.Bytes())
}

func makeMOOF(sequence uint32, tracks []*fragmentedTrack, grouped map[int][]bufferedFragmentSample, trackOffsets map[int]int, dataBase int) ([]byte, error) {
	var p bytes.Buffer
	var mfhdP bytes.Buffer
	putU32(&mfhdP, sequence)
	mfhd, _ := outputFullBox("mfhd", 0, 0, mfhdP.Bytes())
	p.Write(mfhd)
	for _, track := range tracks {
		samples := grouped[track.spec.ID]
		if len(samples) == 0 {
			continue
		}
		traf, err := makeTRAF(track.spec, samples, dataBase+trackOffsets[track.spec.ID])
		if err != nil {
			return nil, err
		}
		p.Write(traf)
	}
	return outputBox("moof", p.Bytes())
}

func makeTRAF(track OutputTrack, samples []bufferedFragmentSample, dataOffset int) ([]byte, error) {
	if dataOffset < 0 || dataOffset > math.MaxInt32 {
		return nil, ErrOutputTooLarge
	}
	var p bytes.Buffer
	var tfhdP bytes.Buffer
	putU32(&tfhdP, uint32(track.ID))
	tfhd, _ := outputFullBox("tfhd", 0, 0x020000, tfhdP.Bytes())
	p.Write(tfhd)
	var tfdtP bytes.Buffer
	putU64(&tfdtP, uint64(samples[0].sample.DTS))
	tfdt, _ := outputFullBox("tfdt", 1, 0, tfdtP.Bytes())
	p.Write(tfdt)
	var trunP bytes.Buffer
	putU32(&trunP, uint32(len(samples)))
	putI32(&trunP, int32(dataOffset))
	for _, buffered := range samples {
		s := buffered.sample
		putU32(&trunP, uint32(s.Duration))
		putU32(&trunP, uint32(buffered.size))
		flags := uint32(0)
		if track.Codec.IsVideo() && !s.Keyframe {
			flags = 0x00010000
		}
		putU32(&trunP, flags)
		composition, ok := checkedSubInt64(s.PTS, s.DTS)
		if !ok || composition < math.MinInt32 || composition > math.MaxInt32 {
			return nil, ErrInvalidOutputSample
		}
		putI32(&trunP, int32(composition))
	}
	trun, err := outputFullBox("trun", 1, 0x000f01, trunP.Bytes())
	if err != nil {
		return nil, err
	}
	p.Write(trun)
	return outputBox("traf", p.Bytes())
}
