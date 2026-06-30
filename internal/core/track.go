package core

import "time"

// TrackKind classifies a track for muxer layout decisions.
type TrackKind uint8

const (
	TrackUnknown TrackKind = iota
	TrackVideo
	TrackAudio
)

// Track describes a single media stream within a muxing session.
//
// A Track is immutable after registration with the muxer; runtime packet
// state lives on Packet values, not on the Track.
type Track struct {
	ID       int
	Kind     TrackKind
	Codec    CodecType
	// Timebase is the codec clock period used to convert packet durations
	// (when known) to time.Duration. For muxing-only use this is mostly
	// informational; the muxer serializes time.Duration directly.
	Timebase time.Duration

	// Video properties (zero for audio tracks).
	Width  int
	Height int

	// Audio properties (zero for video tracks).
	Channels   int
	SampleRate int
}

// IsVideo reports whether the track carries video.
func (t Track) IsVideo() bool { return t.Kind == TrackVideo }

// IsAudio reports whether the track carries audio.
func (t Track) IsAudio() bool { return t.Kind == TrackAudio }
