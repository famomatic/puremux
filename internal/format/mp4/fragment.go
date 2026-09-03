package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"
)

type trafState struct {
	track        *trackState
	baseData     int64
	decodeTime   uint64
	defaultValue trackDefaults
	dataCursor   int64
	haveCursor   bool
}

// NewFragmentReader applies the track/configuration state from init to a
// separate media-segment reader containing moof/mdat boxes.
func NewFragmentReader(init io.Reader, media io.ReadSeeker) (*Reader, error) {
	if init == nil || media == nil {
		return nil, errors.New("mp4: nil fragmented input")
	}
	initBytes, err := io.ReadAll(io.LimitReader(init, 64<<20))
	if err != nil {
		return nil, err
	}
	rd, err := NewReader(bytes.NewReader(initBytes))
	if err != nil {
		return nil, err
	}
	if _, err := media.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	rd.rs = media
	rd.fragments = nil
	rd.fragmentCursor = 0
	if err := rd.parse(); err != nil {
		return nil, err
	}
	rd.sortFragments()
	return rd, nil
}

func (rd *Reader) parseMvex(r io.Reader, b box) error {
	payload, err := readBoundedPayload(r, b)
	if err != nil {
		return err
	}
	mr := bytes.NewReader(payload)
	for mr.Len() > 0 {
		child, err := readBox(mr)
		if err != nil {
			return err
		}
		if child.typ != "trex" {
			if err := skipBox(mr, child); err != nil {
				return err
			}
			continue
		}
		data, err := readBoundedPayload(mr, child)
		if err != nil {
			return err
		}
		if len(data) < 24 {
			return ErrCorrupt
		}
		trackID := binary.BigEndian.Uint32(data[4:8])
		rd.trex[trackID] = trackDefaults{
			descriptionIndex: binary.BigEndian.Uint32(data[8:12]),
			duration:         binary.BigEndian.Uint32(data[12:16]),
			size:             binary.BigEndian.Uint32(data[16:20]),
			flags:            binary.BigEndian.Uint32(data[20:24]),
		}
	}
	return nil
}

func (rd *Reader) parseMoof(b box, moofStart int64) error {
	payload, err := readBoundedPayload(rd.rs, b)
	if err != nil {
		return err
	}
	moofEnd := moofStart + b.size
	mr := bytes.NewReader(payload)
	for mr.Len() > 0 {
		child, err := readBox(mr)
		if err != nil {
			return err
		}
		if child.typ != "traf" {
			if err := skipBox(mr, child); err != nil {
				return err
			}
			continue
		}
		traf, err := readBoundedPayload(mr, child)
		if err != nil {
			return err
		}
		if err := rd.parseTraf(traf, moofStart, moofEnd); err != nil {
			return err
		}
	}
	rd.sortFragments()
	return nil
}

func (rd *Reader) parseTraf(data []byte, moofStart, moofEnd int64) error {
	state := trafState{baseData: moofStart, dataCursor: moofEnd + 8}
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		b, err := readBox(r)
		if err != nil {
			return err
		}
		payload, err := readBoundedPayload(r, b)
		if err != nil {
			return err
		}
		switch b.typ {
		case "tfhd":
			if err := rd.parseTfhd(payload, &state, moofStart); err != nil {
				return err
			}
		case "tfdt":
			if len(payload) < 8 {
				return ErrCorrupt
			}
			if payload[0] == 1 {
				if len(payload) < 12 {
					return ErrCorrupt
				}
				state.decodeTime = binary.BigEndian.Uint64(payload[4:12])
			} else if payload[0] == 0 {
				state.decodeTime = uint64(binary.BigEndian.Uint32(payload[4:8]))
			} else {
				return ErrCorrupt
			}
		case "trun":
			if state.track == nil {
				return errors.New("mp4: trun before tfhd")
			}
			if err := rd.parseTrun(payload, &state); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseTfhd(data []byte, state *trafState, moofStart int64) error {
	if len(data) < 8 {
		return ErrCorrupt
	}
	flags := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	trackID := binary.BigEndian.Uint32(data[4:8])
	for _, track := range rd.tracks {
		if track.info.ID == trackID {
			state.track = track
			break
		}
	}
	if state.track == nil {
		return errors.New("mp4: tfhd references unknown track")
	}
	state.defaultValue = rd.trex[trackID]
	offset := 8
	read32 := func() (uint32, bool) {
		if len(data)-offset < 4 {
			return 0, false
		}
		value := binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4
		return value, true
	}
	if flags&0x000001 != 0 {
		if len(data)-offset < 8 {
			return ErrCorrupt
		}
		value := binary.BigEndian.Uint64(data[offset : offset+8])
		if value > math.MaxInt64 {
			return ErrCorrupt
		}
		state.baseData = int64(value)
		offset += 8
	} else if flags&0x020000 != 0 {
		state.baseData = moofStart
	}
	if flags&0x000002 != 0 {
		value, ok := read32()
		if !ok {
			return ErrCorrupt
		}
		state.defaultValue.descriptionIndex = value
	}
	for _, field := range []struct {
		flag uint32
		dst  *uint32
	}{{0x000008, &state.defaultValue.duration}, {0x000010, &state.defaultValue.size}, {0x000020, &state.defaultValue.flags}} {
		if flags&field.flag != 0 {
			value, ok := read32()
			if !ok {
				return ErrCorrupt
			}
			*field.dst = value
		}
	}
	return nil
}

func (rd *Reader) parseTrun(data []byte, state *trafState) error {
	if len(data) < 8 {
		return ErrCorrupt
	}
	version := data[0]
	flags := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	count := binary.BigEndian.Uint32(data[4:8])
	offset := 8
	if flags&0x000001 != 0 {
		if len(data)-offset < 4 {
			return ErrCorrupt
		}
		relative := int64(int32(binary.BigEndian.Uint32(data[offset : offset+4])))
		if (relative > 0 && state.baseData > math.MaxInt64-relative) || (relative < 0 && state.baseData < math.MinInt64-relative) {
			return ErrCorrupt
		}
		state.dataCursor = state.baseData + relative
		state.haveCursor = true
		offset += 4
	} else if !state.haveCursor {
		state.haveCursor = true
	}
	firstFlags := uint32(0)
	haveFirstFlags := false
	if flags&0x000004 != 0 {
		if len(data)-offset < 4 {
			return ErrCorrupt
		}
		firstFlags = binary.BigEndian.Uint32(data[offset : offset+4])
		haveFirstFlags = true
		offset += 4
	}
	for i := uint32(0); i < count; i++ {
		duration, size, sampleFlags := state.defaultValue.duration, state.defaultValue.size, state.defaultValue.flags
		readField := func(flag uint32, dst *uint32) bool {
			if flags&flag == 0 {
				return true
			}
			if len(data)-offset < 4 {
				return false
			}
			*dst = binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
			return true
		}
		if !readField(0x000100, &duration) || !readField(0x000200, &size) || !readField(0x000400, &sampleFlags) {
			return ErrCorrupt
		}
		if i == 0 && haveFirstFlags && flags&0x000400 == 0 {
			sampleFlags = firstFlags
		}
		composition := int64(0)
		if flags&0x000800 != 0 {
			if len(data)-offset < 4 {
				return ErrCorrupt
			}
			raw := binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
			if version == 1 {
				composition = int64(int32(raw))
			} else if version == 0 {
				composition = int64(raw)
			} else {
				return ErrCorrupt
			}
		}
		if state.decodeTime > math.MaxInt64 || duration == 0 || size == 0 || state.dataCursor < 0 {
			return ErrCorrupt
		}
		dts := int64(state.decodeTime)
		rd.fragments = append(rd.fragments, fragmentSample{track: state.track, dts: dts, pts: dts + composition + state.track.presentationShift, duration: int64(duration), off: state.dataCursor, size: size, keyframe: sampleFlags&0x00010000 == 0})
		if state.decodeTime > math.MaxUint64-uint64(duration) || state.dataCursor > math.MaxInt64-int64(size) {
			return ErrCorrupt
		}
		state.decodeTime += uint64(duration)
		state.dataCursor += int64(size)
	}
	return nil
}

func (rd *Reader) sortFragments() {
	sort.SliceStable(rd.fragments, func(i, j int) bool {
		left, right := rd.fragments[i], rd.fragments[j]
		return timestampLess(left.dts, left.track.timescale, right.dts, right.track.timescale)
	})
}
