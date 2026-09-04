package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
	"github.com/famomatic/puremux/pkg/bitstream/mp3"
)

type audioFrameIndex struct {
	offset   int64
	size     int
	pts      int64
	duration int64
}

type indexedAudioDemuxer struct {
	stateMu    sync.Mutex
	opMu       sync.Mutex
	src        Source
	rs         io.ReadSeeker
	stream     Stream
	info       Info
	frames     []audioFrameIndex
	contextual *contextReadSeeker
	next       int
	closed     bool
}

func openADTS(src Source, rs io.ReadSeeker, contextual *contextReadSeeker) (Demuxer, error) {
	size, err := sourceSize(rs)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, ErrInvalidData
	}
	var frames []audioFrameIndex
	var header [7]byte
	var pts int64
	var sampleRate, channels int
	var config aac.Config
	for offset := int64(0); offset < size; {
		if _, err := rs.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(rs, header[:]); err != nil {
			return nil, err
		}
		frame, ok := core.ParseADTSHeader(header[:])
		if !ok || frame.SampleRate == 0 || frame.Channels == 0 || int64(frame.Length) > size-offset {
			return nil, ErrInvalidData
		}
		headerLength := 7
		if header[1]&1 == 0 {
			headerLength = 9
		}
		if frame.Length < headerLength {
			return nil, ErrInvalidData
		}
		if sampleRate == 0 {
			sampleRate, channels = frame.SampleRate, frame.Channels
			config = aac.Config{AudioObjectType: int(header[2]>>6) + 1, SampleRate: frame.SampleRate, FrequencyIndex: int(header[2]>>2) & 0xf, ChannelConfig: frame.Channels}
		} else if sampleRate != frame.SampleRate || channels != frame.Channels {
			return nil, errors.New("media: ADTS configuration changed")
		}
		frames = append(frames, audioFrameIndex{offset: offset + int64(headerLength), size: frame.Length - headerLength, pts: pts, duration: int64(frame.Samples)})
		pts += int64(frame.Samples)
		offset += int64(frame.Length)
	}
	asc, _ := aac.ASC(config)
	return newIndexedAudio(src, rs, FormatADTS, CodecAAC, sampleRate, channels, asc, CodecConfigASC, frames, nil, contextual), nil
}

func openMP3(src Source, rs io.ReadSeeker, contextual *contextReadSeeker) (Demuxer, error) {
	size, err := sourceSize(rs)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, ErrInvalidData
	}
	offset, metadata, err := readID3v2(rs, size)
	if err != nil {
		return nil, err
	}
	audioEnd, id3v1, err := readID3v1(rs, size, offset)
	if err != nil {
		return nil, err
	}
	for key, value := range id3v1 {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	var frames []audioFrameIndex
	var pts int64
	var sampleRate, channels int
	for offset < audioEnd {
		if _, err := rs.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		var headerBytes [4]byte
		if _, err := io.ReadFull(rs, headerBytes[:]); err != nil {
			return nil, err
		}
		header, err := mp3.ParseHeader(headerBytes[:])
		if err != nil || int64(header.FrameLength) > audioEnd-offset {
			return nil, ErrInvalidData
		}
		if sampleRate == 0 {
			sampleRate, channels = header.SampleRate, header.Channels
		} else if sampleRate != header.SampleRate || channels != header.Channels {
			return nil, errors.New("media: MP3 configuration changed")
		}
		frames = append(frames, audioFrameIndex{offset: offset, size: header.FrameLength, pts: pts, duration: int64(header.Samples)})
		pts += int64(header.Samples)
		offset += int64(header.FrameLength)
	}
	if len(frames) == 0 {
		return nil, ErrInvalidData
	}
	return newIndexedAudio(src, rs, FormatMP3, CodecMP3, sampleRate, channels, nil, CodecConfigUnknown, frames, metadata, contextual), nil
}

func openFLAC(src Source, rs io.ReadSeeker, contextual *contextReadSeeker) (Demuxer, error) {
	size, err := sourceSize(rs)
	if err != nil {
		return nil, err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var magic [4]byte
	if _, err := io.ReadFull(rs, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, ErrInvalidData
	}
	var streamInfo flac.StreamInfo
	var streamInfoRaw []byte
	seenStreamInfo := false
	metadata := make(map[string]string)
	for blockIndex := 0; ; blockIndex++ {
		var header [4]byte
		if _, err := io.ReadFull(rs, header[:]); err != nil {
			return nil, err
		}
		last := header[0]&0x80 != 0
		typ := header[0] & 0x7f
		if blockIndex == 0 && typ != 0 {
			return nil, errors.New("media: FLAC STREAMINFO must be the first metadata block")
		}
		if typ == 0 && seenStreamInfo {
			return nil, errors.New("media: duplicate FLAC STREAMINFO")
		}
		if typ == 127 {
			return nil, errors.New("media: invalid FLAC metadata block type")
		}
		length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		if length > 16<<20 {
			return nil, ErrInvalidData
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(rs, payload); err != nil {
			return nil, err
		}
		switch typ {
		case 0:
			streamInfo, err = flac.ParseStreamInfo(payload)
			if err != nil {
				return nil, err
			}
			seenStreamInfo = true
			streamInfoRaw = append([]byte(nil), payload...)
		case 4:
			if err := parseVorbisComments(payload, metadata); err != nil {
				return nil, err
			}
		}
		if last {
			break
		}
	}
	audioStart, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	frames, err := indexFLACFrames(rs, size, audioStart, streamInfo)
	if err != nil {
		return nil, err
	}
	return newIndexedAudio(src, rs, FormatFLAC, CodecFLAC, streamInfo.SampleRate, streamInfo.Channels, streamInfoRaw, CodecConfigFLACStreamInfo, frames, metadata, contextual), nil
}

func readID3v1(rs io.ReadSeeker, size, audioStart int64) (int64, map[string]string, error) {
	metadata := make(map[string]string)
	if size-audioStart < 128 {
		return size, metadata, nil
	}
	if _, err := rs.Seek(size-128, io.SeekStart); err != nil {
		return 0, nil, err
	}
	var tag [128]byte
	if _, err := io.ReadFull(rs, tag[:]); err != nil {
		return 0, nil, err
	}
	if string(tag[:3]) != "TAG" {
		return size, metadata, nil
	}
	readField := func(data []byte) string {
		return strings.TrimRight(string(data), "\x00 ")
	}
	for key, value := range map[string]string{
		"title":  readField(tag[3:33]),
		"artist": readField(tag[33:63]),
		"album":  readField(tag[63:93]),
	} {
		if value != "" {
			metadata[key] = value
		}
	}
	return size - 128, metadata, nil
}

func newIndexedAudio(src Source, rs io.ReadSeeker, format Format, codec CodecID, sampleRate, channels int, config []byte, configFormat CodecConfigFormat, frames []audioFrameIndex, metadata map[string]string, contextual *contextReadSeeker) *indexedAudioDemuxer {
	duration := int64(0)
	if len(frames) > 0 {
		last := frames[len(frames)-1]
		duration = last.pts + last.duration
	}
	stream := Stream{Index: 0, ID: 0, Type: MediaAudio, Codec: codec, TimeBase: Rational{Num: 1, Den: int64(sampleRate)}, Disposition: DispositionDefault, SampleRate: sampleRate, Channels: channels, Config: CodecConfig{Format: configFormat, Data: append([]byte(nil), config...)}, Metadata: cloneMetadata(metadata)}
	if duration > 0 {
		stream.Duration = KnownTimestamp(duration)
	}
	info := Info{Format: format, FormatName: format.String(), Metadata: cloneMetadata(metadata)}
	if duration > 0 {
		info.Duration, info.DurationKnown = stream.TimeBase.Duration(duration)
	}
	return &indexedAudioDemuxer{src: src, rs: rs, stream: stream, info: info, frames: frames, contextual: contextual}
}

func (d *indexedAudioDemuxer) Streams() []Stream { return cloneStreams([]Stream{d.stream}) }
func (d *indexedAudioDemuxer) Info() Info {
	info := d.info
	info.Metadata = cloneMetadata(info.Metadata)
	return info
}
func (d *indexedAudioDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	if d.next >= len(d.frames) {
		return nil, io.EOF
	}
	frame := d.frames[d.next]
	if _, err := d.rs.Seek(frame.offset, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, frame.size)
	if _, err := io.ReadFull(d.rs, data); err != nil {
		return nil, err
	}
	d.next++
	return &Packet{StreamIndex: 0, Data: data, PTS: KnownTimestamp(frame.pts), DTS: KnownTimestamp(frame.pts), Duration: KnownTimestamp(frame.duration), Flags: PacketKeyframe, Pos: frame.offset}, nil
}
func (d *indexedAudioDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return SeekResult{}, err
	}
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return SeekResult{}, ErrClosed
	}
	if err := validateSeekRequest(req, 1); err != nil {
		return SeekResult{}, err
	}
	if d.contextual != nil {
		d.contextual.setContext(ctx)
	}
	target := req.Target
	if req.StreamIndex == -1 {
		var ok bool
		target, ok = nanosecondTimeBase.Rescale(target, d.stream.TimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	if len(d.frames) == 0 {
		return SeekResult{}, ErrInvalidData
	}
	d.next = len(d.frames) - 1
	if req.Flags&SeekBackward != 0 {
		d.next = 0
		for i, frame := range d.frames {
			if frame.pts > target {
				break
			}
			d.next = i
		}
	} else {
		for i, frame := range d.frames {
			if frame.pts >= target {
				d.next = i
				break
			}
		}
	}
	actual := int64(0)
	if len(d.frames) > 0 {
		actual = d.frames[d.next].pts
	}
	if req.StreamIndex == -1 {
		var ok bool
		actual, ok = d.stream.TimeBase.Rescale(actual, nanosecondTimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
}
func (d *indexedAudioDemuxer) Close() error {
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.stateMu.Unlock()
	return d.src.Close()
}

func sourceSize(rs io.ReadSeeker) (int64, error) {
	current, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	size, err := rs.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = rs.Seek(current, io.SeekStart)
	}
	return size, err
}

func readID3v2(rs io.ReadSeeker, size int64) (int64, map[string]string, error) {
	metadata := make(map[string]string)
	var header [10]byte
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return 0, nil, err
	}
	if _, err := io.ReadFull(rs, header[:]); err != nil {
		if size < 10 {
			return 0, metadata, nil
		}
		return 0, nil, err
	}
	if string(header[:3]) != "ID3" {
		return 0, metadata, nil
	}
	for _, value := range header[6:10] {
		if value&0x80 != 0 {
			return 0, nil, ErrInvalidData
		}
	}
	tagSize := int(header[6])<<21 | int(header[7])<<14 | int(header[8])<<7 | int(header[9])
	if int64(10+tagSize) > size || tagSize > 16<<20 {
		return 0, nil, ErrInvalidData
	}
	body := make([]byte, tagSize)
	if _, err := io.ReadFull(rs, body); err != nil {
		return 0, nil, err
	}
	version := header[3]
	for offset := 0; offset+10 <= len(body); {
		id := string(body[offset : offset+4])
		if id == "\x00\x00\x00\x00" {
			break
		}
		lengthRaw := binary.BigEndian.Uint32(body[offset+4 : offset+8])
		if version != 4 && uint64(lengthRaw) > uint64(len(body)-offset-10) {
			return 0, nil, ErrInvalidData
		}
		length := int(lengthRaw)
		if version == 4 {
			length = int(body[offset+4])<<21 | int(body[offset+5])<<14 | int(body[offset+6])<<7 | int(body[offset+7])
		}
		offset += 10
		if length <= 0 || length > len(body)-offset {
			return 0, nil, ErrInvalidData
		}
		key := map[string]string{"TIT2": "title", "TPE1": "artist", "TALB": "album"}[id]
		if key != "" && length > 1 && body[offset] == 3 {
			metadata[key] = strings.TrimRight(string(body[offset+1:offset+length]), "\x00")
		}
		offset += length
	}
	return int64(10 + tagSize), metadata, nil
}

func parseVorbisComments(data []byte, metadata map[string]string) error {
	offset := 0
	readString := func() (string, bool) {
		if len(data)-offset < 4 {
			return "", false
		}
		lengthRaw := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4
		if uint64(lengthRaw) > uint64(len(data)-offset) {
			return "", false
		}
		length := int(lengthRaw)
		value := string(data[offset : offset+length])
		offset += length
		return value, true
	}
	vendor, ok := readString()
	if !ok {
		return ErrInvalidData
	}
	metadata["vendor"] = vendor
	if len(data)-offset < 4 {
		return ErrInvalidData
	}
	count := binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4
	if uint64(count) > uint64((len(data)-offset)/4) {
		return ErrInvalidData
	}
	for range count {
		comment, ok := readString()
		if !ok {
			return ErrInvalidData
		}
		if split := strings.IndexByte(comment, '='); split > 0 {
			metadata[strings.ToLower(comment[:split])] = comment[split+1:]
		}
	}
	return nil
}

func indexFLACFrames(rs io.ReadSeeker, size, start int64, info flac.StreamInfo) ([]audioFrameIndex, error) {
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(rs, 64<<10)
	var offsets []int64
	position := start
	for position < size {
		peek, _ := reader.Peek(16)
		if len(peek) >= 6 && peek[0] == 0xff && peek[1]&0xfe == 0xf8 {
			if _, err := flac.ParseFrameHeader(peek, info); err == nil {
				offsets = append(offsets, position)
			}
		}
		if _, err := reader.ReadByte(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		position++
	}
	if len(offsets) == 0 && start < size {
		return nil, ErrInvalidData
	}
	frames := make([]audioFrameIndex, len(offsets))
	var pts int64
	for i, offset := range offsets {
		end := size
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		if _, err := rs.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		header := make([]byte, min(16, int(end-offset)))
		if _, err := io.ReadFull(rs, header); err != nil {
			return nil, err
		}
		parsed, err := flac.ParseFrameHeader(header, info)
		if err != nil {
			return nil, err
		}
		duration := int64(parsed.BlockSize)
		frames[i] = audioFrameIndex{offset: offset, size: int(end - offset), pts: pts, duration: duration}
		pts += duration
	}
	return frames, nil
}
