// Package mp3 parses MPEG audio frame headers without decoding audio.
package mp3

import "errors"

type Header struct {
	Version      int // 1, 2, or 25 (MPEG-2.5)
	Layer        int // 1, 2, or 3
	BitRate      int
	SampleRate   int
	Channels     int
	Padding      bool
	FrameLength  int
	Samples      int
	CRCProtected bool
}

var bitrateV1 = [3][16]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
}
var bitrateV2 = [3][16]int{
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
}

func ParseHeader(data []byte) (Header, error) {
	if len(data) < 4 {
		return Header{}, errors.New("mp3: truncated frame header")
	}
	value := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if value>>21 != 0x7ff {
		return Header{}, errors.New("mp3: sync word not found")
	}
	versionBits := int(value>>19) & 3
	layerBits := int(value>>17) & 3
	bitrateIndex := int(value>>12) & 15
	rateIndex := int(value>>10) & 3
	emphasis := int(value & 3)
	if versionBits == 1 || layerBits == 0 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 || emphasis == 2 {
		return Header{}, errors.New("mp3: reserved or free-format header")
	}
	version := map[int]int{0: 25, 2: 2, 3: 1}[versionBits]
	layer := 4 - layerBits
	table := bitrateV2
	if version == 1 {
		table = bitrateV1
	}
	bitrate := table[layer-1][bitrateIndex] * 1000
	rate := [3]int{44100, 48000, 32000}[rateIndex]
	if version == 2 {
		rate /= 2
	} else if version == 25 {
		rate /= 4
	}
	padding := value&(1<<9) != 0
	frameLength, samples := 0, 1152
	switch layer {
	case 1:
		frameLength = (12*bitrate/rate + boolInt(padding)) * 4
		samples = 384
	case 2:
		frameLength = 144*bitrate/rate + boolInt(padding)
	case 3:
		coefficient := 144
		if version != 1 {
			coefficient, samples = 72, 576
		}
		frameLength = coefficient*bitrate/rate + boolInt(padding)
	}
	channels := 2
	if value>>6&3 == 3 {
		channels = 1
	}
	return Header{Version: version, Layer: layer, BitRate: bitrate, SampleRate: rate, Channels: channels, Padding: padding, FrameLength: frameLength, Samples: samples, CRCProtected: value&(1<<16) == 0}, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
