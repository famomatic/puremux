# Puremux Architecture Specification

## 1. Project Overview

`puremux` is a pure Go, native media muxer library.

- **Core Constraint**: Absolutely NO CGO (`CGO_ENABLED=0`), NO external FFmpeg binaries.
- **Scope**: Muxing only. No decoding/encoding or pixel data manipulation. Parses pre-compressed packet headers and serializes them into container formats.
- **Target Containers**: WebM (primary), Matroska/MKV (secondary superset extension)
- **Target Codecs**: VP8, VP9, AV1, Opus
- **Implementation Priority**: WebM first. MKV is a strict superset of WebM and is layered on top, never forked. WebM correctness is the gate for MKV work.

## 2. Core Problem Solved

Provides robust Error Resilience (FFmpeg-level packet preprocessing) for network-streamed packets (e.g., WebRTC, YouTube) that suffer from timestamp inversion or A/V sync drift.

## 3. Directory Layout

```text
puremux/
├── cmd/
│   └── puremux/              # CLI entrypoint
├── pkg/
│   └── puremux/              # Public facade API for external consumers
├── internal/
│   ├── core/                 # Common data structures (Packet, Codec, Track)
│   ├── preprocessor/         # Packet pipeline (Enforcer, Aligner)
│   ├── muxer/                # Muxer interfaces
│   └── format/
│       ├── ebml/             # RFC 8794 EBML low-level parser/builder
│       └── webm/             # WebM/MKV container implementation
```

## 4. Packet Opacity Boundary

The "treat payloads as opaque" rule (AGENTS.md) is precise, not absolute. It forbids **media decoding** — never unpack, decompress, or inspect raw pixel buffers or audio PCM samples. It does NOT forbid reading **codec packet headers** to extract metadata required for muxing decisions.

| Operation                                     | Permitted | Layer            |
| :-------------------------------------------- | :-------- | :--------------- |
| VP9 superframe index parsing                  | Yes       | core (Codec)     |
| AV1 OBU header / temporal_unit keyframe flag | Yes       | core (Codec)     |
| Opus packet frame count read                  | Yes       | core (Codec)     |
| Timestamp / keyframe flag extraction          | Yes       | preprocessor    |
| Decoding compressed frames to pixels          | No        | —                |
| Unpacking audio PCM samples                   | No        | —                |
| Resampling / re-encoding any track            | No        | —                |

Every codec-specific header parse MUST live behind the `CodecKeyframeDetector` interface in `internal/core` (see §5.A). No preprocessor or muxer code may contain inline VP9/AV1 byte probing; it must call through the interface.

## 5. Architectural Layers

### A. Core Data Structures (`internal/core`)

Telemetry primitives with zero/low-allocation design. Must reuse byte buffers via `sync.Pool`.

```go
type Packet struct {
    Data       []byte
    PTS        time.Duration
    DTS        time.Duration
    IsKeyframe bool
    Codec      CodecType
    TrackID    int
}
```

Codec-specific behavior is isolated behind an interface. The preprocessor depends on this, never on concrete codec logic:

```go
// Detects keyframe status and packet boundary metadata from a compressed
// payload's header bytes only. MUST NOT decode the payload.
type CodecKeyframeDetector interface {
    // IsKeyframe inspects the packet header (e.g. VP9 superframe index,
    // AV1 OBU header) and reports whether this packet holds a sync frame.
    IsKeyframe(data []byte) bool
}
```

Each target codec (VP8, VP9, AV1) provides its own implementation registered in `internal/core`. Opus has no keyframe concept and returns the no-op detector.

### B. Preprocessor Layer (`internal/preprocessor`)

Responsible for fixing corrupted packet streams. Operates purely in-memory. Does NOT write to files.

1. **Monotonic Timestamp Enforcer**: Jitter buffer to force chronological ordering and interpolate gaps.
   - The buffer is **bounded and configurable** (`MaxBufferSize`, `MaxBufferDuration`). It is never unbounded.
   - **Overflow policy** (explicit, not implicit): on overflow the oldest out-of-order packet is drop, never silently absorbed into an unbounded queue. The drop MUST be observable via a returned/recorded metric so callers can detect stream degradation.
   - Interpolation fills small gaps only; gaps exceeding a codec-specific threshold are left as discontinuities rather than synthesized.
2. **Keyframe Aligner**: Enforces video stream to start with an IDR/I-Frame. Adjusts audio sync.
   - Audio realignment is **packet-granular only**. The aligner may drop or duplicate whole Opus packets; it MUST NOT trim within a packet, because sample counts are unknown under the opacity rule (§4) and trimming would require decoding.
   - The aligner depends on `CodecKeyframeDetector` (§5.A) — it never probes codec bytes directly.

### C. Muxer Layer (`internal/muxer` & `internal/format/webm`)

Takes corrected packets and writes valid container bytes. Assumes incoming streams are pristine and ordered; it MUST NOT alter timestamps or fix sync (see AGENTS.md separation of concerns).

- **SeekHead + Cues**: When the writer is an `io.Seeker`, the muxer writes a SeekHead that points at the Cues element and the Tracks element for random access.
- **Graceful Closer**: If the underlying `io.Writer` implements `io.Seeker`, it MUST seek back upon `Close()` to overwrite the dummy `Duration` (in the Info element) and `Cues` (Index) that were reserved up front. If not a seeker, uses streaming flags.
- **Streaming Mode (non-seekable)**: When `io.Seeker` is unavailable, `Duration` is left unset (or set to the live `TimecodeScale`-relative placeholder), `Cues` are omitted entirely, and the Segment uses the unknown-size (`0x01FFFFFFFFFFFFFF`) sentinel. This produces a valid live/appendable WebM that players can stream but not seek.

## 6. Concurrency Model

A muxer instance serializes a multi-track stream through a single writer goroutine. Producers (one per track) push `Packet`s onto per-track buffered channels; the single writer drains them in merged timecode order and serializes cluster/block writes. No per-packet mutex is held across I/O — only the channel handoff. This keeps allocation and lock contention out of the hot path and preserves the `sync.Pool` reuse assumption.

The preprocessor runs per-track and is stateful, so each track owns its own Enforcer/Aligner instance; no cross-track shared mutable state exists above the muxer's merge step.

## 7. Implementation Priority & Gates

WebM is implemented and verified first; MKV extensions (extra codecs, additional EBML elements, `Chapters`, `Attachments`) layer on top of the same EBML engine and muxer interfaces. Do not begin MKV-specific work until WebM round-trips a VP9+Opus stream against a reference player.