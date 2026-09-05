package core

import (
	"github.com/famomatic/puremux/pkg/bitstream/opus"
	"time"
)

func OpusPacketSamples(packet []byte) int            { return opus.PacketSamples(packet) }
func OpusPacketDuration(packet []byte) time.Duration { return opus.PacketDuration(packet) }
