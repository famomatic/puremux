package muxer

import (
	"io"

	"github.com/famomatic/puremux/internal/core"
)

// Writer is the sink a Muxer serializes container bytes to. It extends
// io.Writer with optional seeking: when Seek is available the muxer patches
// the reserved Duration/Cues on Close (ARCHITECTURE.md section 5.C).
type Writer interface {
	io.Writer
	// Seek reports whether the sink supports random access. Muxers MUST
	// branch on this rather than type-asserting io.Seeker directly, so that
	// wrapped writers can declare their capability explicitly.
	Seekable() bool
	// Seek, when Seekable() is true, behaves as io.Seeker.Seek.
	Seek(offset int64, whence int) (int64, error)
}

// Config controls muxer behavior that is independent of codec layout.
type Config struct {
	// TimecodeScale is the Matroska TimecodeScale in nanoseconds per
	// timecode unit. Defaults to 1_000_000 (1 ms per unit) when zero.
	TimecodeScale uint64
	// ClusterTargetDuration bounds Cluster element size in time. A new
	// Cluster is started when the current cluster exceeds this. Zero means
	// a single cluster (streaming-friendly default).
	ClusterTargetDuration uint64
}

// Muxer serializes corrected packets into a container stream.
//
// Invariants a Muxer MUST uphold (ARCHITECTURE.md section 5.C):
//   - It assumes incoming packets are pristine and timecode-ordered; it
//     MUST NOT reorder, resample, or alter timestamps.
//   - It writes opaque payloads untouched.
//   - On Close it patches reserved Duration/Cues when Seekable(), else
//     finalizes in streaming mode.
type Muxer interface {
	// AddTrack registers a track before the first WritePacket. Returns the
	// assigned track ID. Must be called before WritePacket.
	AddTrack(t core.Track) (int, error)
	// WritePacket appends a packet belonging to a registered track.
	WritePacket(p *core.Packet) error
	// Close finalizes the container. For seekable writers this patches the
	// reserved Duration and Cues; for non-seekable writers it flushes the
	// streaming-mode tail.
	Close() error
}

// Factory builds a Muxer for a given writer and configuration. Concrete
// format implementations (webm, mkv) provide factories registered by the
// facade.
type Factory func(w Writer, cfg Config) (Muxer, error)
