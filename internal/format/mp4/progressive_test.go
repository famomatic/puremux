package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestProgressiveExactTimingConfigMetadataAndSeek(t *testing.T) {
	// ISO/IEC 14496-12 ctts version 1 stores signed 32-bit composition
	// offsets. Decode timestamps 0/40/80 plus offsets 80/-40/-40 yield
	// presentation timestamps 80/0/40 in decode order.
	ctts := cttsV1([]cttsEntry{{count: 1, offset: 80}, {count: 2, offset: -40}})
	avcc := []byte{1, 0x64, 0, 0x1f, 0xff, 0xe1, 0, 0}
	buildTrack := func(chunk uint32) []byte {
		stbl := mkBox("stbl", bytes.Join([][]byte{
			fullBox("stsd", richVideoSTSD("avc1", 1920, 1080, "avcC", avcc)),
			fullBox("stts", sttsPayload([]sttsEntry{{count: 3, delta: 40}})),
			ctts,
			fullBox("stsz", stszPayload([]uint32{2, 2, 2})),
			fullBox("stsc", stscPayload([]stscEntry{{firstChunk: 1, samplesPerChunk: 3}})),
			fullBox("stco", stcoPayload([]uint32{chunk})),
			fullBox("stss", stssPayload([]uint32{1})),
		}, nil))
		mdhdBody := make([]byte, 20)
		binary.BigEndian.PutUint32(mdhdBody[8:12], 1000)
		binary.BigEndian.PutUint32(mdhdBody[12:16], 120)
		binary.BigEndian.PutUint16(mdhdBody[16:18], 0x15c7) // ISO-639 "eng": 5,14,7.
		tkhdBody := make([]byte, 80)
		binary.BigEndian.PutUint32(tkhdBody[8:12], 7)
		return mkBox("trak", bytes.Join([][]byte{
			fullBox("tkhd", tkhdBody),
			mkBox("mdia", append(fullBox("mdhd", mdhdBody), mkBox("minf", stbl)...)),
		}, nil))
	}
	mdat := []byte{0x65, 1, 0x41, 2, 0x01, 3}
	track := buildTrack(0)
	data, offset := buildRichMP4(track, mdat, "Exact title")
	track = buildTrack(uint32(offset))
	data, _ = buildRichMP4(track, mdat, "Exact title")

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tracks := r.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d", len(tracks))
	}
	trackInfo := tracks[0]
	if trackInfo.ID != 7 || trackInfo.Timescale != 1000 || trackInfo.Duration != 120 || trackInfo.Language != "eng" || trackInfo.Width != 1920 || trackInfo.Height != 1080 {
		t.Fatalf("unexpected track: %+v", trackInfo)
	}
	if trackInfo.CodecConfigType != "avcC" || !bytes.Equal(trackInfo.CodecConfig, avcc) {
		t.Fatalf("config = %q %x", trackInfo.CodecConfigType, trackInfo.CodecConfig)
	}
	if r.Metadata()["title"] != "Exact title" {
		t.Fatalf("metadata = %+v", r.Metadata())
	}
	if duration, scale := r.MovieDuration(); duration != 120 || scale != 1000 {
		t.Fatalf("movie duration = %d/%d", duration, scale)
	}
	wantPTS := []int64{80, 0, 40}
	for i, pts := range wantPTS {
		sample, err := r.NextSample()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if sample.DTS != int64(i*40) || sample.PTS != pts || sample.Duration != 40 || sample.Timescale != 1000 {
			t.Fatalf("sample %d timing = DTS %d PTS %d duration %d scale %d", i, sample.DTS, sample.PTS, sample.Duration, sample.Timescale)
		}
	}
	actual, err := r.SeekNS(1, 100_000_000)
	if err != nil || actual != 80_000_000 {
		t.Fatalf("SeekNS = %d, %v", actual, err)
	}
	if sample, err := r.NextSample(); err != nil || sample.DTS != 0 || !sample.Keyframe {
		t.Fatalf("sample after sync seek = %+v, %v", sample, err)
	}
}

func TestESDSAndEditListBoundaries(t *testing.T) {
	// ISO/IEC 14496-1 descriptor tag 0x05 with a one-byte 7-bit length and
	// MPEG-4 AudioSpecificConfig AAC-LC/44.1kHz/stereo = 0x12 0x10 (MSB-first).
	esds := []byte{0, 0, 0, 0, 0x03, 0x04, 0x05, 0x02, 0x12, 0x10}
	if got := findESDSDecoderConfig(esds); !bytes.Equal(got, []byte{0x12, 0x10}) {
		t.Fatalf("ASC = %x", got)
	}
	if got := findESDSDecoderConfig([]byte{0, 0, 0, 0, 0x05, 0x84, 0x80}); got != nil {
		t.Fatalf("truncated descriptor accepted: %x", got)
	}

	// elst v0 entries are segment_duration u32 + media_time i32 + rate.
	row1 := make([]byte, 12)
	binary.BigEndian.PutUint32(row1[0:4], 20)
	binary.BigEndian.PutUint32(row1[4:8], uint32(0xffffffff)) // empty edit (-1)
	binary.BigEndian.PutUint32(row1[8:12], 0x00010000)
	row2 := make([]byte, 12)
	binary.BigEndian.PutUint32(row2[0:4], 100)
	binary.BigEndian.PutUint32(row2[4:8], 312)
	binary.BigEndian.PutUint32(row2[8:12], 0x00010000)
	elst := fullBox("elst", append([]byte{0, 0, 0, 2}, append(row1, row2...)...))
	rd := &Reader{movieTimescale: 1000}
	state := &trackState{timescale: 48_000}
	if err := rd.parseEdts(bytes.NewReader(elst), box{payload: int64(len(elst))}, state); err != nil {
		t.Fatal(err)
	}
	if state.editLeadMovie != 20 || !state.hasEditMediaTime || state.editMediaTime != 312 {
		t.Fatalf("edit state = %+v", state)
	}

	badCTTS := append([]byte{1, 0, 0, 0}, []byte{0, 0, 0, 2}...)
	if err := rd.parseCtts(bytes.NewReader(badCTTS), box{payload: int64(len(badCTTS))}, state); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated ctts = %v", err)
	}
}

func cttsV1(entries []cttsEntry) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(len(entries)))
	for _, entry := range entries {
		row := make([]byte, 8)
		binary.BigEndian.PutUint32(row[0:4], entry.count)
		binary.BigEndian.PutUint32(row[4:8], uint32(int32(entry.offset)))
		payload = append(payload, row...)
	}
	header := []byte{1, 0, 0, 0}
	return mkBox("ctts", append(header, payload...))
}

func richVideoSTSD(entryType string, width, height uint16, configType string, config []byte) []byte {
	body := make([]byte, 78)
	binary.BigEndian.PutUint16(body[6:8], 1)
	binary.BigEndian.PutUint16(body[24:26], width)
	binary.BigEndian.PutUint16(body[26:28], height)
	body = append(body, mkBox(configType, config)...)
	entry := make([]byte, 8)
	binary.BigEndian.PutUint32(entry[0:4], uint32(8+len(body)))
	copy(entry[4:8], entryType)
	entry = append(entry, body...)
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, 1)
	return append(payload, entry...)
}

func buildRichMP4(track, mdat []byte, title string) ([]byte, int) {
	mvhdBody := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhdBody[8:12], 1000)
	binary.BigEndian.PutUint32(mvhdBody[12:16], 120)
	dataPayload := append(make([]byte, 8), []byte(title)...)
	metadata := mkBox("udta", mkBox("meta", append(make([]byte, 4), mkBox("ilst", mkBox("\xa9nam", mkBox("data", dataPayload)))...)))
	moov := mkBox("moov", bytes.Join([][]byte{fullBox("mvhd", mvhdBody), track, metadata}, nil))
	file := append(mkBox("ftyp", ftypPayload()), moov...)
	offset := len(file) + 8
	file = append(file, mkBox("mdat", mdat)...)
	return file, offset
}
