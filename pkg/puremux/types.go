package puremux

import "github.com/famomatic/puremux/internal/core"

// Public aliases for the internal packet/codec types so external consumers
// can construct packets and name codecs without importing internal/core
// (which the Go toolchain forbids). These are type aliases, not copies: a
// *Packet built here flows through the pipeline without conversion.

// Packet is one opaque compressed media packet. See Session.WritePacket.
type Packet = core.Packet

// CodecType identifies the compressed codec carried by a Packet or Track.
type CodecType = core.CodecType

// Codec constants re-exported for external callers.
const (
	CodecUnknown = core.CodecUnknown
	CodecVP8     = core.CodecVP8
	CodecVP9     = core.CodecVP9
	CodecAV1     = core.CodecAV1
	CodecOpus    = core.CodecOpus
	CodecVorbis  = core.CodecVorbis
	CodecFLAC    = core.CodecFLAC
	CodecAAC     = core.CodecAAC
	CodecH264    = core.CodecH264
	CodecHEVC    = core.CodecHEVC
	CodecMP3     = core.CodecMP3
)
