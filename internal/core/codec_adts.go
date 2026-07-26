package core

import "time"

// ADTS (Audio Data Transport Stream) header parsing for AAC elementary
// streams (ISO/IEC 14496-3 §1.A.2.2). This is codec *packet header* parsing,
// permitted under the opacity rule (ARCHITECTURE.md §4): only the 7/9-byte
// fixed+variable header is read; the raw AAC payload is never decoded.
//
// Layout (MSB-first):
//
//	byte0: syncword hi (0xFF)
//	byte1: syncword lo (0xF) | ID(1) | layer(2, always 00) | protection_absent(1)
//	byte2: profile(2) | sampling_frequency_index(4) | private_bit(1) | channel_config bit2(1)
//	byte3: channel_config bits1-0(2) | original(1) | home(1) | copyright_id(1) |
//	       copyright_start(1) | frame_length bits12-11(2)
//	byte4: frame_length bits10-3(8)
//	byte5: frame_length bits2-0(3) | buffer_fullness bits10-6(5)
//	byte6: buffer_fullness bits5-0(6) | number_of_raw_data_blocks(2)
//
// frame_length counts the whole frame including the header.

// adtsHeaderLen is the fixed+variable ADTS header size without CRC.
const adtsHeaderLen = 7

// adtsSampleRates maps sampling_frequency_index to Hz (index 0-12; 13-15 are
// reserved and yield 0).
var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// ADTSFrameInfo describes one ADTS frame's header fields needed for muxing.
type ADTSFrameInfo struct {
	// Length is the total frame length in bytes, including the header.
	Length int
	// SampleRate in Hz decoded from sampling_frequency_index (0 if reserved).
	SampleRate int
	// Channels from channel_configuration (0 when signalled out-of-band via PCE).
	Channels int
	// Samples is the PCM sample count per channel carried by this frame:
	// 1024 * (number_of_raw_data_blocks + 1).
	Samples int
}

// Duration returns the frame's play time, or 0 when the sample rate is
// unknown (reserved index).
func (i ADTSFrameInfo) Duration() time.Duration {
	if i.SampleRate <= 0 {
		return 0
	}
	return time.Duration(i.Samples) * time.Second / time.Duration(i.SampleRate)
}

// ParseADTSHeader parses one ADTS header at the start of b. Returns ok=false
// when b is too short, the syncword is absent, or the frame length is smaller
// than the header itself.
func ParseADTSHeader(b []byte) (ADTSFrameInfo, bool) {
	if len(b) < adtsHeaderLen {
		return ADTSFrameInfo{}, false
	}
	if b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
		return ADTSFrameInfo{}, false
	}
	// layer must be 00 for ADTS.
	if b[1]&0x06 != 0 {
		return ADTSFrameInfo{}, false
	}
	length := (int(b[3]&0x03) << 11) | (int(b[4]) << 3) | (int(b[5]) >> 5)
	if length < adtsHeaderLen {
		return ADTSFrameInfo{}, false
	}
	freqIdx := (b[2] >> 2) & 0x0F
	chanCfg := ((b[2] & 0x01) << 2) | (b[3] >> 6)
	rawBlocks := int(b[6] & 0x03)
	return ADTSFrameInfo{
		Length:     length,
		SampleRate: adtsSampleRates[freqIdx],
		Channels:   int(chanCfg),
		Samples:    1024 * (rawBlocks + 1),
	}, true
}

// ForEachADTSFrame walks b, which may hold several concatenated ADTS frames,
// invoking fn for each complete frame (header included). Bytes that do not
// start a valid frame are skipped one at a time (resync scan). A trailing
// truncated frame is not emitted. Iteration stops early when fn returns false.
func ForEachADTSFrame(b []byte, fn func(frame []byte, info ADTSFrameInfo) bool) {
	for i := 0; i+adtsHeaderLen <= len(b); {
		info, ok := ParseADTSHeader(b[i:])
		if !ok {
			i++
			continue
		}
		if i+info.Length > len(b) {
			return // truncated tail; wait for more bytes upstream
		}
		if !fn(b[i:i+info.Length], info) {
			return
		}
		i += info.Length
	}
}
