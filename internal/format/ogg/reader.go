// Package ogg implements bounded Ogg page and Ogg Opus packet parsing.
package ogg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"

	"github.com/famomatic/puremux/internal/core"
)

const maxOggPageSize = 27 + 255 + 255*255

type OpusHead struct {
	Version         uint8
	Channels        uint8
	PreSkip         uint16
	InputSampleRate uint32
	OutputGain      int16
	MappingFamily   uint8
	StreamCount     uint8
	CoupledCount    uint8
	ChannelMapping  []byte
	Packet          []byte
}

type Packet struct {
	Data           []byte
	PTS            int64
	Duration       int64
	Position       int64
	EOS            bool
	DiscardSamples int64
}

type pageIndex struct {
	offset        int64
	granule       uint64
	flags         byte
	headersBefore int
}

type page struct {
	offset   int64
	flags    byte
	granule  uint64
	serial   uint32
	sequence uint32
	lacing   []byte
	body     []byte
}

type queuedPacket struct {
	data     []byte
	position int64
}

type Reader struct {
	mu sync.Mutex
	rs io.ReadSeeker

	size       int64
	serial     uint32
	head       OpusHead
	tags       map[string]string
	pages      []pageIndex
	duration   int64
	firstAudio int64

	headerPackets int
	partial       []byte
	partialPos    int64
	pending       []Packet
	closed        bool
	audioEnd      int64
	haveAudio     bool
}

func NewReader(rs io.ReadSeeker) (*Reader, error) {
	if rs == nil {
		return nil, errors.New("ogg: nil reader")
	}
	r := &Reader{rs: rs, tags: make(map[string]string), firstAudio: -1}
	if err := r.scan(); err != nil {
		return nil, err
	}
	if _, err := r.rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	r.headerPackets = 0
	return r, nil
}

func (r *Reader) Head() OpusHead {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.head
	h.ChannelMapping = append([]byte(nil), h.ChannelMapping...)
	h.Packet = append([]byte(nil), h.Packet...)
	return h
}

func (r *Reader) Tags() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.tags))
	for key, value := range r.tags {
		out[key] = value
	}
	return out
}

func (r *Reader) DurationSamples() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.duration
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.partial = nil
	r.pending = nil
	return nil
}

func (r *Reader) NextPacket(ctx context.Context) (*Packet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("ogg: reader closed")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(r.pending) > 0 {
			packet := r.pending[0]
			r.pending = r.pending[1:]
			return &packet, nil
		}
		position, err := r.rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if position >= r.size {
			if len(r.partial) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, io.EOF
		}
		p, err := readPage(r.rs, r.size)
		if err != nil {
			return nil, err
		}
		if p.serial != r.serial {
			continue
		}
		packets, err := r.consumePage(p)
		if err != nil {
			return nil, err
		}
		if len(packets) == 0 {
			continue
		}
		var total int64
		durations := make([]int64, len(packets))
		for i := range packets {
			durations[i] = int64(core.OpusPacketSamples(packets[i].data))
			if durations[i] <= 0 || total > math.MaxInt64-durations[i] {
				return nil, errors.New("ogg: invalid Opus packet duration")
			}
			total += durations[i]
		}
		end := int64(r.head.PreSkip) + total
		if p.granule != math.MaxUint64 {
			if p.granule > math.MaxInt64 {
				return nil, errors.New("ogg: granule position overflow")
			}
			end = int64(p.granule)
		}
		start := end - total
		if p.flags&0x04 != 0 {
			if r.haveAudio {
				start = r.audioEnd
			} else if start < 0 {
				start = 0
			}
			if end < start || end-start > total || (!r.haveAudio && end < int64(r.head.PreSkip)) {
				return nil, errors.New("ogg: invalid EOS granule")
			}
		} else if start < 0 {
			return nil, errors.New("ogg: invalid initial granule")
		}
		if start > math.MaxInt64-total {
			return nil, errors.New("ogg: audio clock overflow")
		}
		r.audioEnd, r.haveAudio = start+total, true
		for i, packet := range packets {
			pts := start - int64(r.head.PreSkip)
			r.pending = append(r.pending, Packet{
				Data:           packet.data,
				PTS:            pts,
				Duration:       durations[i],
				Position:       packet.position,
				EOS:            p.flags&0x04 != 0 && i == len(packets)-1,
				DiscardSamples: min(durations[i], max(int64(0), start+durations[i]-end)),
			})
			start += durations[i]
		}
	}
}

func (r *Reader) SeekSamples(ctx context.Context, target int64) (int64, error) {
	return r.SeekSamplesWithDirection(ctx, target, true)
}

// SeekSamplesWithDirection positions at the nearest indexed audio page in
// the requested direction. Ogg Opus packets are independently decodable, so
// every indexed packet is an eligible sync point.
func (r *Reader) SeekSamplesWithDirection(ctx context.Context, target int64, backward bool) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errors.New("ogg: reader closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if target < 0 {
		target = 0
	}
	choice := -1
	lastAudio := -1
	pageStart := func(index int) int64 {
		for i := index - 1; i >= 0; i-- {
			granule := r.pages[i].granule
			if granule == math.MaxUint64 || granule <= uint64(r.head.PreSkip) {
				continue
			}
			if granule-uint64(r.head.PreSkip) > math.MaxInt64 {
				return math.MaxInt64
			}
			return int64(granule - uint64(r.head.PreSkip))
		}
		return 0
	}
	for i := range r.pages {
		if r.pages[i].offset >= r.firstAudio {
			if choice < 0 {
				choice = i
			}
			lastAudio = i
		}
	}
	if backward {
		for i, p := range r.pages {
			if p.offset < r.firstAudio || pageStart(i) > target {
				continue
			}
			choice = i
		}
	} else {
		found := false
		for i, p := range r.pages {
			if p.offset < r.firstAudio || pageStart(i) < target {
				continue
			}
			choice = i
			found = true
			break
		}
		if !found {
			choice = lastAudio
		}
	}
	if choice < 0 {
		return 0, errors.New("ogg: no audio seek page")
	}
	for choice > 0 && r.pages[choice].flags&0x01 != 0 {
		choice--
	}
	if _, err := r.rs.Seek(r.pages[choice].offset, io.SeekStart); err != nil {
		return 0, err
	}
	r.partial = nil
	r.pending = nil
	r.headerPackets = r.pages[choice].headersBefore
	r.audioEnd, r.haveAudio = 0, false
	for i := choice - 1; i >= 0; i-- {
		if r.pages[i].headersBefore >= 2 && r.pages[i].granule != math.MaxUint64 && r.pages[i].granule <= math.MaxInt64 {
			r.audioEnd, r.haveAudio = int64(r.pages[i].granule), true
			break
		}
	}
	return pageStart(choice), nil
}

func (r *Reader) scan() error {
	cur, err := r.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	size, err := r.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	r.size = size
	if _, err := r.rs.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var serialSet bool
	var expectedSequence uint32
	var headers [][]byte
	var partial []byte
	lastGranule := uint64(math.MaxUint64)
	for {
		position, _ := r.rs.Seek(0, io.SeekCurrent)
		if position >= size {
			break
		}
		p, err := readPage(r.rs, size)
		if err != nil {
			return err
		}
		if !serialSet {
			if p.flags&0x02 == 0 {
				return errors.New("ogg: first page is not BOS")
			}
			r.serial = p.serial
			serialSet = true
			expectedSequence = p.sequence
		}
		if p.serial != r.serial {
			continue
		}
		if p.sequence != expectedSequence {
			return errors.New("ogg: page sequence discontinuity")
		}
		expectedSequence++
		r.pages = append(r.pages, pageIndex{offset: p.offset, granule: p.granule, flags: p.flags, headersBefore: len(headers)})
		complete, nextPartial, _, err := splitPagePackets(p, partial, 0)
		if err != nil {
			return err
		}
		partial = nextPartial
		for _, packet := range complete {
			if len(headers) < 2 {
				headers = append(headers, packet.data)
				if len(headers) == 2 {
					r.firstAudio = p.offset
				}
			}
		}
		if p.granule != math.MaxUint64 {
			lastGranule = p.granule
		}
	}
	if len(partial) != 0 {
		return io.ErrUnexpectedEOF
	}
	if len(headers) < 2 {
		return errors.New("ogg: missing Opus headers")
	}
	head, err := parseOpusHead(headers[0])
	if err != nil {
		return err
	}
	tags, err := parseOpusTags(headers[1])
	if err != nil {
		return err
	}
	r.head, r.tags = head, tags
	if lastGranule != math.MaxUint64 && lastGranule >= uint64(r.head.PreSkip) && lastGranule-uint64(r.head.PreSkip) <= math.MaxInt64 {
		r.duration = int64(lastGranule - uint64(r.head.PreSkip))
	}
	// firstAudio is the page after the tags page for the standard layout. If
	// audio shared that page, rescan from BOS and skip the two header packets.
	if r.firstAudio < 0 || r.firstAudio >= size {
		r.firstAudio = 0
	}
	if _, err := r.rs.Seek(cur, io.SeekStart); err != nil {
		return err
	}
	return nil
}

func (r *Reader) consumePage(p page) ([]queuedPacket, error) {
	complete, partial, partialPos, err := splitPagePackets(p, r.partial, r.partialPos)
	if err != nil {
		return nil, err
	}
	r.partial, r.partialPos = partial, partialPos
	var audio []queuedPacket
	for _, packet := range complete {
		if r.headerPackets < 2 {
			r.headerPackets++
			continue
		}
		audio = append(audio, packet)
	}
	return audio, nil
}

func splitPagePackets(p page, partial []byte, partialPos int64) ([]queuedPacket, []byte, int64, error) {
	continued := p.flags&0x01 != 0
	if continued != (len(partial) != 0) {
		return nil, nil, 0, errors.New("ogg: inconsistent continued-packet flag")
	}
	bodyOffset := 0
	var complete []queuedPacket
	current := partial
	position := partialPos
	for _, segment := range p.lacing {
		length := int(segment)
		if bodyOffset+length > len(p.body) {
			return nil, nil, 0, io.ErrUnexpectedEOF
		}
		if current == nil {
			position = p.offset + 27 + int64(len(p.lacing)) + int64(bodyOffset)
		}
		if len(current) > (16<<20)-length {
			return nil, nil, 0, errors.New("ogg: continued packet exceeds size limit")
		}
		current = append(current, p.body[bodyOffset:bodyOffset+length]...)
		bodyOffset += length
		if segment < 255 {
			complete = append(complete, queuedPacket{data: current, position: position})
			current = nil
			position = 0
		}
	}
	if bodyOffset != len(p.body) {
		return nil, nil, 0, errors.New("ogg: lacing table does not cover page body")
	}
	return complete, current, position, nil
}

func readPage(rs io.ReadSeeker, size int64) (page, error) {
	offset, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return page{}, err
	}
	var fixed [27]byte
	if _, err := io.ReadFull(rs, fixed[:]); err != nil {
		return page{}, err
	}
	if !bytes.Equal(fixed[:4], []byte("OggS")) || fixed[4] != 0 {
		return page{}, errors.New("ogg: invalid capture pattern or version")
	}
	segments := int(fixed[26])
	lacing := make([]byte, segments)
	if _, err := io.ReadFull(rs, lacing); err != nil {
		return page{}, err
	}
	bodySize := 0
	for _, length := range lacing {
		bodySize += int(length)
	}
	if 27+segments+bodySize > maxOggPageSize || offset+int64(27+segments+bodySize) > size {
		return page{}, io.ErrUnexpectedEOF
	}
	body := make([]byte, bodySize)
	if _, err := io.ReadFull(rs, body); err != nil {
		return page{}, err
	}
	encoded := make([]byte, 0, 27+segments+bodySize)
	encoded = append(encoded, fixed[:]...)
	encoded = append(encoded, lacing...)
	encoded = append(encoded, body...)
	wantCRC := binary.LittleEndian.Uint32(encoded[22:26])
	clear(encoded[22:26])
	if got := CRC(encoded); got != wantCRC {
		return page{}, fmt.Errorf("ogg: CRC mismatch at offset %d", offset)
	}
	return page{
		offset:   offset,
		flags:    fixed[5],
		granule:  binary.LittleEndian.Uint64(fixed[6:14]),
		serial:   binary.LittleEndian.Uint32(fixed[14:18]),
		sequence: binary.LittleEndian.Uint32(fixed[18:22]),
		lacing:   lacing,
		body:     body,
	}, nil
}

// CRC computes the Ogg page checksum using polynomial 0x04C11DB7, MSB-first.
func CRC(data []byte) uint32 {
	var crc uint32
	for _, value := range data {
		crc ^= uint32(value) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func parseOpusHead(packet []byte) (OpusHead, error) {
	if len(packet) < 19 || !bytes.Equal(packet[:8], []byte("OpusHead")) {
		return OpusHead{}, errors.New("ogg: invalid OpusHead")
	}
	head := OpusHead{
		Version:         packet[8],
		Channels:        packet[9],
		PreSkip:         binary.LittleEndian.Uint16(packet[10:12]),
		InputSampleRate: binary.LittleEndian.Uint32(packet[12:16]),
		OutputGain:      int16(binary.LittleEndian.Uint16(packet[16:18])),
		MappingFamily:   packet[18],
		Packet:          append([]byte(nil), packet...),
	}
	if head.Version > 15 || head.Channels == 0 {
		return OpusHead{}, errors.New("ogg: unsupported OpusHead version or channels")
	}
	if head.MappingFamily == 0 {
		if head.Channels > 2 || len(packet) != 19 {
			return OpusHead{}, errors.New("ogg: invalid mapping-family-0 OpusHead")
		}
		return head, nil
	}
	needed := 21 + int(head.Channels)
	if len(packet) != needed {
		return OpusHead{}, io.ErrUnexpectedEOF
	}
	head.StreamCount, head.CoupledCount = packet[19], packet[20]
	head.ChannelMapping = append([]byte(nil), packet[21:needed]...)
	if head.StreamCount == 0 || head.CoupledCount > head.StreamCount || int(head.StreamCount)+int(head.CoupledCount) > 255 {
		return OpusHead{}, errors.New("ogg: invalid Opus channel mapping")
	}
	decodedChannels := int(head.StreamCount) + int(head.CoupledCount)
	for _, index := range head.ChannelMapping {
		if index != 255 && int(index) >= decodedChannels {
			return OpusHead{}, errors.New("ogg: Opus channel map index out of range")
		}
	}
	return head, nil
}

func parseOpusTags(packet []byte) (map[string]string, error) {
	if len(packet) < 16 || !bytes.Equal(packet[:8], []byte("OpusTags")) {
		return nil, errors.New("ogg: invalid OpusTags")
	}
	offset := 8
	vendorLengthRaw := binary.LittleEndian.Uint32(packet[offset : offset+4])
	offset += 4
	if len(packet)-offset < 4 || uint64(vendorLengthRaw) > uint64(len(packet)-offset-4) {
		return nil, io.ErrUnexpectedEOF
	}
	vendorLength := int(vendorLengthRaw)
	tags := map[string]string{"vendor": string(packet[offset : offset+vendorLength])}
	offset += vendorLength
	count := binary.LittleEndian.Uint32(packet[offset : offset+4])
	offset += 4
	if uint64(count) > uint64((len(packet)-offset)/4) {
		return nil, errors.New("ogg: excessive OpusTags comment count")
	}
	for range count {
		if len(packet)-offset < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		lengthRaw := binary.LittleEndian.Uint32(packet[offset : offset+4])
		offset += 4
		if uint64(lengthRaw) > uint64(len(packet)-offset) {
			return nil, io.ErrUnexpectedEOF
		}
		length := int(lengthRaw)
		comment := string(packet[offset : offset+length])
		offset += length
		if split := strings.IndexByte(comment, '='); split > 0 {
			tags[strings.ToLower(comment[:split])] = comment[split+1:]
		}
	}
	return tags, nil
}
