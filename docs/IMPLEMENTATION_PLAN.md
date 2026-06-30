# Puremux Implementation Ledger

This document tracks the live progress of the `puremux` project.
**Rule:** AI agents MUST update the Status, Date, and Notes immediately after completing a task.

## Design Decisions Locked (2026-06-29)

These decisions are binding on all subsequent implementation work. They are
recorded here so the Progress Ledger and the architecture doc stay in sync.

1. **Packet opacity is scoped, not absolute.** Codec *packet header* parsing
   (VP9 superframe index, AV1 OBU header, Opus frame count) is permitted and
   lives behind the `CodecKeyframeDetector` interface in `internal/core`.
   Media *decoding* (pixels / PCM) is permanently forbidden. See
   ARCHITECTURE.md §4.
2. **Jitter buffer is bounded.** `MaxBufferSize` and `MaxBufferDuration` are
   configurable. Overflow drops the oldest out-of-order packet and records a
   metric. No unbounded absorption. See ARCHITECTURE.md §5.B.
3. **Audio realignment is packet-granular only.** The Keyframe Aligner may
   drop or duplicate whole Opus packets but MUST NOT trim within a packet.
4. **SeekHead + Cues + Duration patch** for seekable writers; streaming
   sentinel + omitted Cues for non-seekable writers. See ARCHITECTURE.md §5.C.
5. **Single-writer concurrency model.** Per-track channels merge into one
   writer goroutine. Preprocessors are per-track and stateful.
6. **WebM first, MKV as superset.** No MKV-specific work until a VP9+Opus
   WebM round-trips against a reference player.

## Progress Ledger

| Phase / Task                                          | Status  | Updated Date | Assignee   | Notes                                              |
| :---------------------------------------------------- | :------ | :----------- | :--------- | :------------------------------------------------- |
| **Phase 0: Design & Docs**                            |         |              |            |                                                    |
| Architecture + Implementation Plan scaffold          | Done    | 2026-06-29   | glm-5.2    | Initial docs from project kickoff                  |
| Architecture reinforcement (opacity, jitter, concur) | Done    | 2026-06-29   | glm-5.2    | Added §4-§7: opacity table, CodecKeyframeDetector, bounded jitter, streaming mode, concurrency model, WebM-first gate |
| **Phase 1: Setup & Core**                             |         |              |            |                                                    |
| Go Module Init & Directory Scaffold                  | Done    | 2026-06-30   | glm-5.2    | module famomatic/puremux (renamed from github.com/mjmst/puremux), Go 1.23.5; scaffolded cmd/pkg/internal tree |
| Define internal/core (Packet, Codecs)                | Done    | 2026-06-30   | glm-5.2    | Packet/CodecType/Track/sync.Pool; VP8/VP9/AV1 detectors behind CodecKeyframeDetector. Verification: spec-derived bytes, MSB-first confirmed for AV1, boundary cases (nil/empty/truncated/forbidden-bit) covered, no panics. CGO_ENABLED=0 build+vet+tests green |
| Define base Interfaces (Muxer, Preprocessor)         | Done    | 2026-06-30   | glm-5.2    | muxer.Muxer/Writer/Factory; preprocessor.Preprocessor/Config/Metrics; CGO_ENABLED=0 build + vet + tests green |
| **Phase 2: EBML Engine**                             |         |              |            |                                                    |
| Implement EBML Element Writer (VINT, etc.)            | Done    | 2026-06-30   | glm-5.2    | ebml: VINT encode/decode, ElementID (id includes marker bit, classes 1-4 verified vs WebM IDs), EncodeElement/UnknownSize, writer helpers. Verification: RFC 8794 spec-derived bytes, width 1-8 roundtrips, unknown-size sentinels, boundary cases, no panics |
| Implement EBML Parser (for seeking/patching)          | Done    | 2026-06-30   | glm-5.2    | ebml: ReadHeader (ID+size decode), PatchSize (reserved width overwrite for graceful closer), Header bookkeeping. Verification: spec-derived Cluster/Segment headers, patch roundtrip, overflow + bounds cases |
| **Phase 3: Preprocessor**                            |         |              |            |                                                    |
| Monotonic Timestamp Enforcer                          | Done    | 2026-06-30   | glm-5.2    | preprocessor.Enforcer: bounded jitter buffer (MaxBufferSize/MaxBufferDuration), time-based hold + Flush commit, overflow drops oldest + DroppedOverflow metric, late-packet drop, strict monotonicity nudge. Verification: out-of-order reorder, late drop, overflow drop (no Flush, large window), reset, monotonic order confirmed |
| Keyframe Aligner                                      | Done    | 2026-06-30   | glm-5.2    | preprocessor.Aligner: video drops until first keyframe (via CodecKeyframeDetector, never inline byte probe), packet-granular audio drop (whole-packet only, no trim per §5.B), SetVideoSyncStart propagation. Verification: drop-until-keyframe, pass-after-keyframe, audio packet-granular drop with data-intact check (trim forbidden), audio-only passthrough, reset |
| **Phase 4: WebM Muxer**                              |         |              |            |                                                    |
| WebM Header Initialization                            | Done    | 2026-06-30   | glm-5.2    | webm: EBML header (doctype webm v4), BeginSegment (seekable reserved 8-byte size / streaming unknown sentinel), WriteInfo (TimecodeScale + reserved Duration float slot). Verification: spec-derived Segment ID 0x18538067, reserved size width-8 = 0x01+7zeros, streaming sentinel = 0x01+7*0xFF, Duration payload offset confirmed via mock seeker |
| Cluster & Block Serialization                         | Done    | 2026-06-30   | glm-5.2    | webm: SimpleBlock (TrackNumber VINT + int16 BE timecode + flags + opaque payload), ClusterWriter (BeginCluster/WriteSimpleBlock/Close, reserved 4-byte size patched on close, streaming unknown sentinel). Verification: spec-derived bytes (track1=0x81, tc100=0x0064, keyframe=0x80, neg tc -100=0xFF9C, wide track 200=0x40 0xC8, int16 bounds), size patch roundtrip, double-close safe |
| Graceful Closer (Seek & Patch Duration/Cues)          | Done    | 2026-06-30   | glm-5.2    | webm: PatchDuration (IEEE-754 double BE overwrite), WriteCues (CuePoint/CueTrackPositions), WriteSeekHead (Info/Tracks/Cues pointers, streaming omits Cues), WriteTracks (TrackEntry/Video/Audio). Verification: end-to-end test generates complete WebM (EBML header+Segment+Info+Tracks+Cluster+SimpleBlock+Cues), Segment size patched, Duration=1.0s patched to 0x3FF0000000000000, SeekHead pointers, spec-derived values confirmed |
| **Phase 5: CLI & Facade**                            |         |              |            |                                                    |
| Build `pkg/puremux` (Public API)                     | Done    | 2026-06-30   | glm-5.2    | pkg/puremux.Session: NewSession (auto seekable/non-seekable), AddTrack, WritePacket (Enforcer->Aligner->Cluster pipeline, per-track preprocessors, video sync propagation), Close (PatchDuration/Cues/PatchSegmentSize seekable; streaming sentinel otherwise). Verification: seekable VP9+Opus end-to-end (webm/V_VP9/A_OPUS/Cluster/SimpleBlock present, 240B output), streaming unknown-size sentinel, unknown-track error, idempotent close |
| Build `cmd/puremux` (CLI wrapper)                     | Done    | 2026-06-30   | glm-5.2    | cmd/puremux: minimal entrypoint with -h/--help usage; thin shell over the library API. CGO_ENABLED=0 build green |
