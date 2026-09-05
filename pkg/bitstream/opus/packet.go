package opus

import "time"

// PacketSamples returns the duration of an Opus packet in samples at the
// mandatory 48 kHz RTP clock. It reads only the RFC 6716 TOC header and
// returns 0 for a truncated or forbidden packet duration.
func PacketSamples(packet []byte) int {
	if len(packet) == 0 {
		return 0
	}
	config := packet[0] >> 3
	var perFrame int
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		perFrame = []int{480, 960, 1920, 2880}[config&3]
	case config < 16: // Hybrid: 10, 20 ms
		perFrame = []int{480, 960}[config&1]
	default: // CELT: 2.5, 5, 10, 20 ms
		perFrame = []int{120, 240, 480, 960}[config&3]
	}

	frames := 1
	switch packet[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(packet) < 2 {
			return 0
		}
		frames = int(packet[1] & 0x3f)
		if frames == 0 {
			return 0
		}
	}
	samples := perFrame * frames
	// RFC 6716 section 3.2.5 limits a packet to 120 ms.
	if samples > 5760 {
		return 0
	}
	return samples
}

func PacketDuration(packet []byte) time.Duration {
	samples := PacketSamples(packet)
	if samples == 0 {
		return 0
	}
	return time.Duration(samples) * time.Second / 48000
}
