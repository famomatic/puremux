// Package flac parses FLAC stream and frame headers without decoding subframes.
package flac

import (
	"encoding/binary"
	"errors"
	"io"
)

type StreamInfo struct {
	MinBlockSize  uint16
	MaxBlockSize  uint16
	SampleRate    int
	Channels      int
	BitsPerSample int
	TotalSamples  uint64
	MD5           [16]byte
}

func ParseStreamInfo(data []byte) (StreamInfo, error) {
	if len(data) != 34 {
		return StreamInfo{}, errors.New("flac: STREAMINFO must be 34 bytes")
	}
	var info StreamInfo
	info.MinBlockSize = binary.BigEndian.Uint16(data[0:2])
	info.MaxBlockSize = binary.BigEndian.Uint16(data[2:4])
	packed := binary.BigEndian.Uint64(data[10:18])
	info.SampleRate = int(packed >> 44)
	info.Channels = int(packed>>41&7) + 1
	info.BitsPerSample = int(packed>>36&31) + 1
	info.TotalSamples = packed & 0xfffffffff
	copy(info.MD5[:], data[18:34])
	if info.MinBlockSize == 0 || info.MaxBlockSize < info.MinBlockSize || info.SampleRate == 0 {
		return StreamInfo{}, errors.New("flac: invalid STREAMINFO")
	}
	return info, nil
}

type FrameHeader struct {
	VariableBlock bool
	BlockSize     int
	SampleRate    int
	Channels      int
	BitsPerSample int
	Number        uint64
	HeaderLength  int
}

func ParseFrameHeader(data []byte, stream StreamInfo) (FrameHeader, error) {
	if len(data) < 6 || data[0] != 0xff || data[1]&0xfe != 0xf8 || data[1]&0x02 != 0 {
		return FrameHeader{}, errors.New("flac: invalid frame sync or reserved bit")
	}
	blockCode, rateCode := int(data[2]>>4), int(data[2]&15)
	channelAssignment, sampleCode := int(data[3]>>4), int(data[3]>>1&7)
	if blockCode == 0 || rateCode == 15 || channelAssignment > 10 || data[3]&1 != 0 || sampleCode == 3 || sampleCode == 7 {
		return FrameHeader{}, errors.New("flac: reserved frame header field")
	}
	offset := 4
	number, used, ok := readUTF8Uint(data[offset:])
	if !ok {
		return FrameHeader{}, io.ErrUnexpectedEOF
	}
	offset += used
	blockSize := stream.MaxBlockSize
	switch blockCode {
	case 1:
		blockSize = 192
	case 2, 3, 4, 5:
		blockSize = uint16(576 << (blockCode - 2))
	case 6:
		if offset >= len(data) {
			return FrameHeader{}, io.ErrUnexpectedEOF
		}
		blockSize = uint16(data[offset]) + 1
		offset++
	case 7:
		if len(data)-offset < 2 {
			return FrameHeader{}, io.ErrUnexpectedEOF
		}
		blockSize = binary.BigEndian.Uint16(data[offset:offset+2]) + 1
		offset += 2
	default:
		blockSize = uint16(256 << (blockCode - 8))
	}
	sampleRate := stream.SampleRate
	switch rateCode {
	case 12:
		if offset >= len(data) {
			return FrameHeader{}, io.ErrUnexpectedEOF
		}
		sampleRate = int(data[offset]) * 1000
		offset++
	case 13:
		if len(data)-offset < 2 {
			return FrameHeader{}, io.ErrUnexpectedEOF
		}
		sampleRate = int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
	case 14:
		if len(data)-offset < 2 {
			return FrameHeader{}, io.ErrUnexpectedEOF
		}
		sampleRate = int(binary.BigEndian.Uint16(data[offset:offset+2])) * 10
		offset += 2
	default:
		if rateCode > 0 {
			sampleRate = [...]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rateCode]
		}
	}
	if offset >= len(data) || CRC8(data[:offset]) != data[offset] {
		return FrameHeader{}, errors.New("flac: frame header CRC-8 mismatch")
	}
	channels := channelAssignment + 1
	if channelAssignment >= 8 {
		channels = 2
	}
	bits := stream.BitsPerSample
	if sampleCode != 0 {
		bits = map[int]int{1: 8, 2: 12, 4: 16, 5: 20, 6: 24}[sampleCode]
	}
	return FrameHeader{VariableBlock: data[1]&1 != 0, BlockSize: int(blockSize), SampleRate: sampleRate, Channels: channels, BitsPerSample: bits, Number: number, HeaderLength: offset + 1}, nil
}

func CRC8(data []byte) byte {
	var crc byte
	for _, value := range data {
		crc ^= value
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func readUTF8Uint(data []byte) (uint64, int, bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	first := data[0]
	if first&0x80 == 0 {
		return uint64(first), 1, true
	}
	leading := 0
	for mask := byte(0x80); first&mask != 0; mask >>= 1 {
		leading++
	}
	if leading < 2 || leading > 7 || len(data) < leading {
		return 0, 0, false
	}
	value := uint64(first & (0x7f >> leading))
	for i := 1; i < leading; i++ {
		if data[i]&0xc0 != 0x80 {
			return 0, 0, false
		}
		value = value<<6 | uint64(data[i]&0x3f)
	}
	return value, leading, true
}
