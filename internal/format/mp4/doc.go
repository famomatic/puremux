// Package mp4 is a pure-Go ISO Base Media File Format (ISOBMFF) demuxer.
//
// It reads MP4/M4A container framing (boxes) and yields opaque media samples
// for remuxing into WebM/MKV. It performs NO media decoding; codec payloads
// are copied verbatim (ARCHITECTURE.md section 4 opacity invariant).
//
// This file is the Phase C stub; the full box walker is implemented in
// reader.go. Tests construct spec-accurate ftyp/moov/mdat byte streams.
package mp4
