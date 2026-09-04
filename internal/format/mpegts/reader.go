package mpegts

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sort"

	"github.com/famomatic/puremux/internal/core"
	"github.com/famomatic/puremux/pkg/bitstream/aac"
	"github.com/famomatic/puremux/pkg/bitstream/mp3"
)

type InputTrack struct {
	PID        uint16
	Codec      core.CodecType
	Timescale  int
	SampleRate int
	Channels   int
	Config     []byte
}

type InputPacket struct {
	Track    int
	Data     []byte
	PTS      int64
	DTS      int64
	Duration int64
	Keyframe bool
	Offset   int64
}

type InputReader struct {
	tracks  []InputTrack
	packets []InputPacket
	next    int
}

// StreamingInputReader incrementally parses a non-seekable transport stream.
// It buffers at most one in-progress PES per PID plus packets discovered while
// waiting for the PMT's audio configuration; it never waits for source EOF.
type StreamingInputReader struct {
	r           io.Reader
	offset      int64
	pmtPID      uint16
	streamTypes map[uint16]byte
	continuity  map[uint16]byte
	seenCC      map[uint16]bool
	active      map[uint16]*pesBuffer
	trackByPID  map[uint16]int
	tracks      []InputTrack
	pending     []InputPacket
	eof         bool
}

const maxStreamingPESSize = 64 << 20

func NewStreamingInputReader(r io.Reader) (*StreamingInputReader, error) {
	if r == nil {
		return nil, errors.New("mpegts: nil streaming reader")
	}
	s := &StreamingInputReader{
		r:           r,
		pmtPID:      0xffff,
		streamTypes: make(map[uint16]byte),
		continuity:  make(map[uint16]byte),
		seenCC:      make(map[uint16]bool),
		active:      make(map[uint16]*pesBuffer),
		trackByPID:  make(map[uint16]int),
	}
	for !s.ready() {
		if err := s.pump(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *StreamingInputReader) Tracks() []InputTrack {
	out := append([]InputTrack(nil), s.tracks...)
	for i := range out {
		out[i].Config = append([]byte(nil), out[i].Config...)
	}
	return out
}

func (s *StreamingInputReader) NextPacket() (InputPacket, error) {
	for len(s.pending) == 0 {
		if s.eof {
			return InputPacket{}, io.EOF
		}
		if err := s.pump(); err != nil {
			return InputPacket{}, err
		}
	}
	p := s.pending[0]
	copy(s.pending, s.pending[1:])
	s.pending[len(s.pending)-1] = InputPacket{}
	s.pending = s.pending[:len(s.pending)-1]
	return p, nil
}

func (s *StreamingInputReader) ready() bool {
	if len(s.tracks) == 0 || len(s.pending) == 0 {
		return false
	}
	for _, track := range s.tracks {
		if (track.Codec == core.CodecAAC || track.Codec == core.CodecMP3) && track.SampleRate == 0 {
			return false
		}
	}
	return true
}

func (s *StreamingInputReader) pump() error {
	var packet [188]byte
	n, err := io.ReadFull(s.r, packet[:])
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			s.eof = true
			return s.flushActive()
		}
		return err
	}
	offset := s.offset
	s.offset += 188
	return s.consumePacket(packet[:], offset)
}

func (s *StreamingInputReader) consumePacket(packet []byte, offset int64) error {
	if packet[0] != 0x47 || packet[1]&0x80 != 0 {
		return errors.New("mpegts: invalid sync or transport error")
	}
	start := packet[1]&0x40 != 0
	pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
	afc := packet[3] >> 4 & 3
	if afc == 0 {
		return errors.New("mpegts: reserved adaptation control")
	}
	payloadOffset := 4
	discontinuity := false
	if afc&2 != 0 {
		length := int(packet[4])
		if length > 183 || 5+length > len(packet) {
			return errors.New("mpegts: adaptation field overruns packet")
		}
		if length > 0 {
			discontinuity = packet[5]&0x80 != 0
		}
		payloadOffset = 5 + length
	}
	if afc&1 == 0 || payloadOffset == len(packet) {
		return nil
	}
	cc := packet[3] & 15
	if s.seenCC[pid] && !discontinuity && cc != (s.continuity[pid]+1)&15 {
		return errors.New("mpegts: continuity counter gap")
	}
	s.seenCC[pid], s.continuity[pid] = true, cc
	payload := packet[payloadOffset:]
	if pid == 0 {
		section, err := psiSection(payload, start)
		if err != nil {
			return err
		}
		if len(section) > 0 {
			s.pmtPID, err = parsePAT(section)
			return err
		}
		return nil
	}
	if pid == s.pmtPID {
		section, err := psiSection(payload, start)
		if err != nil {
			return err
		}
		if len(section) > 0 {
			if err := parsePMT(section, s.streamTypes); err != nil {
				return err
			}
			return s.initializeTracks(section)
		}
		return nil
	}
	if _, known := s.trackByPID[pid]; !known {
		return nil
	}
	if start {
		if previous := s.active[pid]; previous != nil {
			if err := s.finishPES(previous); err != nil {
				return err
			}
		}
		s.active[pid] = &pesBuffer{pid: pid, offset: offset}
	}
	if current := s.active[pid]; current != nil {
		if len(current.data) > maxStreamingPESSize-len(payload) {
			return errors.New("mpegts: streaming PES exceeds size limit")
		}
		current.data = append(current.data, payload...)
	}
	return nil
}

func (s *StreamingInputReader) initializeTracks(section []byte) error {
	if len(s.tracks) != 0 {
		return nil
	}
	programInfo := int(section[10]&15)<<8 | int(section[11])
	for offset := 12 + programInfo; offset+5 <= len(section)-4; {
		typ := section[offset]
		pid := uint16(section[offset+1]&0x1f)<<8 | uint16(section[offset+2])
		length := int(section[offset+3]&15)<<8 | int(section[offset+4])
		offset += 5 + length
		codec := streamCodec(typ)
		if codec == core.CodecUnknown {
			continue
		}
		s.trackByPID[pid] = len(s.tracks)
		s.tracks = append(s.tracks, InputTrack{PID: pid, Codec: codec, Timescale: 90000})
	}
	return nil
}

func (s *StreamingInputReader) finishPES(pes *pesBuffer) error {
	index := s.trackByPID[pes.pid]
	track := s.tracks[index]
	packets, update, err := parsePES(pes, index, track.Codec)
	if err != nil {
		return err
	}
	if update.SampleRate != 0 {
		update.PID, update.Codec = pes.pid, track.Codec
		if track.SampleRate != 0 && (track.SampleRate != update.SampleRate || track.Channels != update.Channels || track.Timescale != update.Timescale || !bytes.Equal(track.Config, update.Config)) {
			return errors.New("mpegts: elementary stream configuration changed")
		}
		if track.SampleRate == 0 {
			s.tracks[index] = update
		}
	}
	s.pending = append(s.pending, packets...)
	return nil
}

func (s *StreamingInputReader) flushActive() error {
	completed := make([]*pesBuffer, 0, len(s.active))
	for _, pes := range s.active {
		completed = append(completed, pes)
	}
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].offset < completed[j].offset })
	for _, pes := range completed {
		if err := s.finishPES(pes); err != nil {
			return err
		}
	}
	s.active = make(map[uint16]*pesBuffer)
	if len(s.tracks) == 0 || len(s.pending) == 0 {
		return errors.New("mpegts: no supported elementary packets")
	}
	return nil
}

type pesBuffer struct {
	pid    uint16
	offset int64
	data   []byte
}

// NewInputReader indexes an aligned single-program MPEG-2 transport stream.
// It parses transport/PES and codec framing only; elementary payloads are not
// decoded.
func NewInputReader(r io.Reader) (*InputReader, error) {
	var raw []byte
	buffer := make([]byte, 188*256)
	for {
		n, err := r.Read(buffer)
		raw = append(raw, buffer[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if len(raw) == 0 || len(raw)%188 != 0 {
		return nil, errors.New("mpegts: input is not 188-byte packet aligned")
	}
	pmtPID := uint16(0xffff)
	streamTypes := make(map[uint16]byte)
	continuity := make(map[uint16]byte)
	seenCC := make(map[uint16]bool)
	active := make(map[uint16]*pesBuffer)
	var completed []*pesBuffer
	for offset := 0; offset < len(raw); offset += 188 {
		packet := raw[offset : offset+188]
		if packet[0] != 0x47 || packet[1]&0x80 != 0 {
			return nil, errors.New("mpegts: invalid sync or transport error")
		}
		start := packet[1]&0x40 != 0
		pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
		afc := packet[3] >> 4 & 3
		if afc == 0 {
			return nil, errors.New("mpegts: reserved adaptation control")
		}
		payloadOffset := 4
		discontinuity := false
		if afc&2 != 0 {
			length := int(packet[4])
			if length > 183 || 5+length > len(packet) {
				return nil, errors.New("mpegts: adaptation field overruns packet")
			}
			if length > 0 {
				discontinuity = packet[5]&0x80 != 0
			}
			payloadOffset = 5 + length
		}
		if afc&1 == 0 || payloadOffset == len(packet) {
			continue
		}
		cc := packet[3] & 15
		if seenCC[pid] && !discontinuity && cc != (continuity[pid]+1)&15 {
			return nil, errors.New("mpegts: continuity counter gap")
		}
		seenCC[pid], continuity[pid] = true, cc
		payload := packet[payloadOffset:]
		if pid == 0 {
			section, err := psiSection(payload, start)
			if err != nil {
				return nil, err
			}
			if len(section) > 0 {
				pmtPID, err = parsePAT(section)
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		if pid == pmtPID {
			section, err := psiSection(payload, start)
			if err != nil {
				return nil, err
			}
			if len(section) > 0 {
				if err := parsePMT(section, streamTypes); err != nil {
					return nil, err
				}
			}
			continue
		}
		if _, known := streamTypes[pid]; !known {
			continue
		}
		if start {
			if previous := active[pid]; previous != nil {
				completed = append(completed, previous)
			}
			active[pid] = &pesBuffer{pid: pid, offset: int64(offset)}
		}
		if current := active[pid]; current != nil {
			current.data = append(current.data, payload...)
		}
	}
	for _, current := range active {
		completed = append(completed, current)
	}
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].offset < completed[j].offset })
	reader := &InputReader{}
	trackByPID := make(map[uint16]int)
	for _, pes := range completed {
		typ := streamTypes[pes.pid]
		codec := streamCodec(typ)
		if codec == core.CodecUnknown {
			continue
		}
		index, ok := trackByPID[pes.pid]
		if !ok {
			index = len(reader.tracks)
			trackByPID[pes.pid] = index
			reader.tracks = append(reader.tracks, InputTrack{PID: pes.pid, Codec: codec, Timescale: 90000})
		}
		packets, update, err := parsePES(pes, index, codec)
		if err != nil {
			return nil, err
		}
		if update.SampleRate != 0 {
			update.PID = pes.pid
			update.Codec = codec
			existing := reader.tracks[index]
			if existing.SampleRate != 0 && (existing.SampleRate != update.SampleRate || existing.Channels != update.Channels || existing.Timescale != update.Timescale || !bytes.Equal(existing.Config, update.Config)) {
				return nil, errors.New("mpegts: elementary stream configuration changed")
			}
			if existing.SampleRate == 0 {
				reader.tracks[index] = update
			}
		}
		reader.packets = append(reader.packets, packets...)
	}
	if len(reader.tracks) == 0 || len(reader.packets) == 0 {
		return nil, errors.New("mpegts: no supported elementary packets")
	}
	return reader, nil
}

func (r *InputReader) Tracks() []InputTrack {
	out := append([]InputTrack(nil), r.tracks...)
	for i := range out {
		out[i].Config = append([]byte(nil), out[i].Config...)
	}
	return out
}

func (r *InputReader) NextPacket() (InputPacket, error) {
	if r.next >= len(r.packets) {
		return InputPacket{}, io.EOF
	}
	p := r.packets[r.next]
	r.next++
	p.Data = append([]byte(nil), p.Data...)
	return p, nil
}

func (r *InputReader) Seek(track int, target int64) (int64, error) {
	return r.SeekWithFlags(track, target, true, false)
}

// SeekWithFlags positions at an eligible packet in the requested direction.
// Audio packets are sync points; video packets require keyframe=true unless
// any is requested.
func (r *InputReader) SeekWithFlags(track int, target int64, backward, any bool) (int64, error) {
	if track < 0 || track >= len(r.tracks) {
		return 0, errors.New("mpegts: track index out of range")
	}
	chosen := -1
	for i, packet := range r.packets {
		if packet.Track != track || (!any && !packet.Keyframe && r.tracks[track].Codec != core.CodecAAC && r.tracks[track].Codec != core.CodecMP3) {
			continue
		}
		if backward {
			if packet.PTS <= target {
				chosen = i
			}
		} else if packet.PTS >= target {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		if backward {
			for i, packet := range r.packets {
				if packet.Track == track && (any || packet.Keyframe || r.tracks[track].Codec == core.CodecAAC || r.tracks[track].Codec == core.CodecMP3) {
					chosen = i
					break
				}
			}
		} else {
			for i := len(r.packets) - 1; i >= 0; i-- {
				packet := r.packets[i]
				if packet.Track == track && (any || packet.Keyframe || r.tracks[track].Codec == core.CodecAAC || r.tracks[track].Codec == core.CodecMP3) {
					chosen = i
					break
				}
			}
		}
	}
	if chosen < 0 {
		return 0, errors.New("mpegts: no eligible seek packet")
	}
	r.next = chosen
	return r.packets[chosen].PTS, nil
}

func psiSection(payload []byte, start bool) ([]byte, error) {
	if !start {
		return nil, nil
	}
	if len(payload) == 0 || int(payload[0])+1 > len(payload) {
		return nil, errors.New("mpegts: invalid PSI pointer")
	}
	payload = payload[1+int(payload[0]):]
	if len(payload) < 3 {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(payload[1]&15)<<8 | int(payload[2])
	if length < 4 || 3+length > len(payload) {
		return nil, io.ErrUnexpectedEOF
	}
	section := payload[:3+length]
	if crc32MPEG(section) != 0 {
		return nil, errors.New("mpegts: PSI CRC mismatch")
	}
	return section, nil
}

func parsePAT(section []byte) (uint16, error) {
	if len(section) < 12 || section[0] != 0x00 {
		return 0, errors.New("mpegts: invalid PAT")
	}
	for offset := 8; offset+4 <= len(section)-4; offset += 4 {
		program := binary.BigEndian.Uint16(section[offset : offset+2])
		if program != 0 {
			return uint16(section[offset+2]&0x1f)<<8 | uint16(section[offset+3]), nil
		}
	}
	return 0, errors.New("mpegts: PAT has no program")
}

func parsePMT(section []byte, streams map[uint16]byte) error {
	if len(section) < 16 || section[0] != 0x02 {
		return errors.New("mpegts: invalid PMT")
	}
	programInfo := int(section[10]&15)<<8 | int(section[11])
	offset := 12 + programInfo
	for offset+5 <= len(section)-4 {
		typ := section[offset]
		pid := uint16(section[offset+1]&0x1f)<<8 | uint16(section[offset+2])
		length := int(section[offset+3]&15)<<8 | int(section[offset+4])
		offset += 5
		if length > len(section)-4-offset {
			return io.ErrUnexpectedEOF
		}
		streams[pid] = typ
		offset += length
	}
	return nil
}

func streamCodec(typ byte) core.CodecType {
	switch typ {
	case 0x0f:
		return core.CodecAAC
	case 0x03, 0x04:
		return core.CodecMP3
	case 0x1b:
		return core.CodecH264
	case 0x24:
		return core.CodecHEVC
	default:
		return core.CodecUnknown
	}
}

func parsePES(pes *pesBuffer, track int, codec core.CodecType) ([]InputPacket, InputTrack, error) {
	data := pes.data
	if len(data) < 9 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[6]&0xc0 != 0x80 {
		return nil, InputTrack{}, errors.New("mpegts: invalid PES header")
	}
	packetLength := int(binary.BigEndian.Uint16(data[4:6]))
	if packetLength != 0 {
		end := 6 + packetLength
		if end > len(data) {
			return nil, InputTrack{}, io.ErrUnexpectedEOF
		}
		data = data[:end]
	}
	headerLength := int(data[8])
	if 9+headerLength > len(data) {
		return nil, InputTrack{}, io.ErrUnexpectedEOF
	}
	pts, dts := int64(0), int64(0)
	flags := data[7] >> 6
	if flags == 1 {
		return nil, InputTrack{}, errors.New("mpegts: forbidden PTS_DTS_flags value")
	}
	if flags == 2 || flags == 3 {
		if headerLength < 5 {
			return nil, InputTrack{}, io.ErrUnexpectedEOF
		}
		var err error
		prefix := byte(2)
		if flags == 3 {
			prefix = 3
		}
		pts, err = decodeTimestamp(data[9:14], prefix)
		if err != nil {
			return nil, InputTrack{}, err
		}
		dts = pts
	}
	if flags == 3 {
		if headerLength < 10 {
			return nil, InputTrack{}, io.ErrUnexpectedEOF
		}
		var err error
		dts, err = decodeTimestamp(data[14:19], 1)
		if err != nil {
			return nil, InputTrack{}, err
		}
	}
	payload := data[9+headerLength:]
	trackInfo := InputTrack{Codec: codec, Timescale: 90000}
	switch codec {
	case core.CodecAAC:
		var packets []InputPacket
		var sampleOffset int64
		for len(payload) > 0 {
			header, err := aac.ParseADTS(payload)
			if err != nil {
				return nil, InputTrack{}, err
			}
			if trackInfo.SampleRate == 0 {
				trackInfo.SampleRate, trackInfo.Channels, trackInfo.Timescale = header.SampleRate, header.ChannelConfig, header.SampleRate
				trackInfo.Config, _ = aac.ASC(aac.Config{AudioObjectType: header.Profile, SampleRate: header.SampleRate, FrequencyIndex: header.FrequencyIndex, ChannelConfig: header.ChannelConfig})
			}
			packetPTS := pts*int64(header.SampleRate)/90000 + sampleOffset
			packets = append(packets, InputPacket{Track: track, Data: append([]byte(nil), payload[header.HeaderLength:header.FrameLength]...), PTS: packetPTS, DTS: packetPTS, Duration: int64(header.Samples), Keyframe: true, Offset: pes.offset})
			sampleOffset += int64(header.Samples)
			payload = payload[header.FrameLength:]
		}
		return packets, trackInfo, nil
	case core.CodecMP3:
		var packets []InputPacket
		var sampleOffset int64
		for len(payload) > 0 {
			header, err := mp3.ParseHeader(payload)
			if err != nil || header.FrameLength > len(payload) {
				return nil, InputTrack{}, errors.New("mpegts: malformed MP3 frame")
			}
			if trackInfo.SampleRate == 0 {
				trackInfo.SampleRate, trackInfo.Channels, trackInfo.Timescale = header.SampleRate, header.Channels, header.SampleRate
			}
			packetPTS := pts*int64(header.SampleRate)/90000 + sampleOffset
			packets = append(packets, InputPacket{Track: track, Data: append([]byte(nil), payload[:header.FrameLength]...), PTS: packetPTS, DTS: packetPTS, Duration: int64(header.Samples), Keyframe: true, Offset: pes.offset})
			sampleOffset += int64(header.Samples)
			payload = payload[header.FrameLength:]
		}
		return packets, trackInfo, nil
	default:
		if len(payload) == 0 {
			return nil, InputTrack{}, errors.New("mpegts: empty PES payload")
		}
		return []InputPacket{{Track: track, Data: append([]byte(nil), payload...), PTS: pts, DTS: dts, Keyframe: annexBKeyframe(codec, payload), Offset: pes.offset}}, trackInfo, nil
	}
}

func decodeTimestamp(data []byte, prefix byte) (int64, error) {
	if len(data) < 5 || data[0]>>4 != prefix || data[0]&1 == 0 || data[2]&1 == 0 || data[4]&1 == 0 {
		return 0, errors.New("mpegts: malformed PES timestamp markers")
	}
	value := int64(data[0]>>1&7)<<30 | int64(binary.BigEndian.Uint16(data[1:3])>>1)<<15 | int64(binary.BigEndian.Uint16(data[3:5])>>1)
	return value, nil
}

func annexBKeyframe(codec core.CodecType, data []byte) bool {
	for i := 0; i+4 < len(data); i++ {
		start := 0
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			start = i + 3
		} else if i+4 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			start = i + 4
		}
		if start == 0 || start >= len(data) {
			continue
		}
		if codec == core.CodecH264 && data[start]&0x1f == 5 {
			return true
		}
		if codec == core.CodecHEVC {
			typ := data[start] >> 1 & 0x3f
			if typ >= 16 && typ <= 23 {
				return true
			}
		}
	}
	return false
}
