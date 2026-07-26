# Changelog

All notable changes to puremux are documented here. Versions are git tags on
`main`; the module has no `v1` stability promise yet.

## v0.0.10 — 2026-07-27

### Fixed

- **MPEG-TS: `DefaultConfig()` yielded non-monotonic PES DTS.**
  `MinMonotonicStep` defaults to 0 (a 1 ns monotonic-DTS nudge), which rounds
  to **0 ticks** at the 90 kHz PES clock, so a duplicate-stamped burst collapsed
  to identical PES DTS — the exact "non monotonically increasing dts" failure,
  for any caller who did not manually set the step. `NewSession` now clamps
  `MinMonotonicStep` to at least one 90 kHz tick (11112 ns) when the output
  container is MPEG-TS.
- **`toTicks` int64 overflow on long streams.** The ns→90 kHz conversion
  multiplied by 90000, overflowing int64 after ~28.5 h (a 24/7 live session
  hits this), wrapping to a clamped-to-zero timestamp. Reduced the fraction to
  9/100000 so the multiply now overflows only after ~32 years.
- **`WriteADTS` missing closed-session guard (pooled-packet leak).** Unlike
  `WriteVideo`, it did not check the closed flag, so a call after `Close`
  acquired a pooled packet that `WritePacket` then declined without releasing.
  Added the guard.
- **HEVC POC: RSV_IRAP_VCL22/23 misparsed.** The IRAP range stopped at
  `CRA_NUT` (21), so reserved IRAP types 22/23 skipped
  `no_output_of_prior_pics_flag`, misaligning the slice header (wrong POC →
  wrong PTS). Extended the IRAP range through 23 per H.265 §7.3.6.1.

### Tests

- Real-data replay (`TestWriteVideoRealBFrameStream`, opt-in
  `PUREMUX_TS_SAMPLE=<ts>`): replays a captured B-frame transport stream's
  access units through `WriteVideo` and asserts strictly monotonic PES DTS,
  DTS ≤ PTS, and decode order preserved — guards the v0.0.9 POC fix against real
  bitstreams a synthetic fixture might miss.
- `TestMPEGTSDefaultConfigMonotonicDTS`, `TestWriteADTSAfterCloseGuarded`, and a
  strengthened `TestMuxerTimestampBaseAndClamp` (exact value at 30 h to catch
  the overflow).

## v0.0.9 — 2026-07-27

### Fixed

- **`WriteVideo` + MPEG-TS: fatal non-monotonic timeline on H.264/HEVC with
  B-frames — fixed with POC-derived presentation timestamps (the complete
  fix, implemented for both H.264 and HEVC; not the minimum monotonic-DTS
  fallback).** The live caller feeds decode-order access units stamped with
  a monotonic *decode* clock (`PTS == DTS == t`). For bitstreams with
  B-frames the display order differs from decode order (it lives in the
  bitstream picture order count, never in `t`), so emitting `t` as the PES
  PTS produced presentation timestamps that were non-monotonic in display
  order: ffmpeg failed with "Application provided invalid, non monotonically
  increasing dts to muxer in stream 0" and mpv flooded "Invalid video
  timestamp" and dropped frames (~80 of every 240 in field captures).

  Now each AU's POC is parsed from the SPS/PPS/slice headers and the PES
  carries: **DTS = the input decode clock, untouched** (strictly monotonic
  by construction) and **PTS = the frame's display slot on that clock**
  (picture with display rank d gets slot `t[d + D]`, D = observed reorder
  depth — the causality delay). PES packets carry a distinct PTS/DTS pair
  exactly when the stream reorders. Verified against real x264 (240-frame
  b-pyramid) and x265 (120-frame) streams: ffprobe reports 0 non-monotonic
  `pkt_dts_time` and 0 DTS>PTS on video and audio; decoded display-order
  PTS is strictly increasing at exact frame spacing; `ffmpeg -f null`
  decodes with zero warnings.

  Correction of the two prior releases' scoping (v0.0.7/v0.0.8): the field
  failure was never a WriteVideoReordered problem — the source has no
  per-frame presentation timestamps at all. `WriteVideoReordered` remains
  for callers that DO hold decode-order presentation stamps; callers on a
  monotonic decode clock (the live P2P shape) should use `WriteVideo`,
  which now handles B-frames correctly.

- **Keyframe Aligner dropped standalone parameter sets.** A pre-keyframe
  video AU carrying only SPS/PPS (no coded slice) was discarded with the
  undecodable pre-IDR frames; if the IDR did not repeat the parameter sets
  in-band the whole stream was undecodable. Configuration-only AUs (new
  `core.CodecConfigOnlyDetector`, H.264 + HEVC) are now held (bounded queue
  of 8, oldest dropped) and replayed immediately ahead of the first
  keyframe.

- **MPEG-TS `AddTrack` could silently alias PES stream_ids.** The 17th
  video (0xE0+16) or 33rd audio (0xC0+32) track overflowed its ISO 13818-1
  stream_id range into foreign ids; both now return an error at the range
  bound.

### Added

- `core.PictureOrderParser` (`NewPictureOrderParser`): stateful H.264/HEVC
  picture-order-count decoding from AU bytes — in-band SPS/PPS caching,
  Exp-Golomb/RBSP reader with emulation-prevention removal, H.264 POC type
  0 (MSB wraparound per §8.2.1.1, reference-only prev tracking,
  `delta_pic_order_cnt_bottom` min rule) and type 2, HEVC §8.3.1 (prevTid0
  tracking, IDR/BLA/first-CRA resets, mid-stream CRA continuity), IDR
  epoch numbering. POC type 1 and malformed slices report per-AU
  "no picture" so callers degrade to decode-order presentation. Validated
  bit-exact against spec-derived fixtures and real encoder output.
- `internal/preprocessor.PresentationSynthesizer`: the display-timeline
  mirror of the DTS synthesizer. Startup probe (8 pictures) collapses
  reorder-free streams to a zero-steady-latency passthrough byte-identical
  to v0.0.8; B-frame streams get a bounded lookahead window (observed
  pyramid depth + margin, capped at the 16-frame DPB bound) with exact
  display ranking, extrapolated anchor slots (median frame interval), and a
  per-frame `PTS >= DTS` clamp so a scrambled/duplicate-stamped startup
  burst degrades locally instead of corrupting the timeline. Verified over
  60 shuffled/duplicated burst seeds at the full-session level —
  deterministic, every output frame strictly monotonic DTS at 90 kHz with
  `DTS <= PTS`.

### Audited (no defect found)

Full-path correctness sweep of the live/preprocessor/TS code: ADTS parsing
(resync scan, truncated-tail withholding, per-frame duration on mid-stream
config changes), Enforcer stable equal-DTS insertion and PTS-preserving
nudges, packet-pool ownership on every emit/drop/flush path, the
payload-copy contract on all four write entry points (the POC parser reads
the caller's buffer only synchronously and caches decoded values, never
slices), PES header flags/length (0 = unbounded for oversized video AUs),
PCR placement and 33-bit masking, continuity counters, PAT/PMT cadence,
zero-length AUs, keyframe-less streams (bounded audio pending queue), and
`go test -race` across the suite (the library spawns no goroutines;
single-writer contract documented).

### Notes

- Video presentation necessarily lags the shared live clock by the reorder
  depth (a late-delivered frame cannot present at its capture slot); the
  offset is constant and small (2–3 frame intervals for typical B-pyramids).
- `WriteVideo` startup latency for H.264/HEVC TS tracks is a one-time
  8-picture probe; B-frame streams then run at a constant lookahead of
  observed-depth+2 pictures. Reorder-free streams keep zero steady-state
  latency and byte-identical output.

## v0.0.8 — 2026-07-27

### Fixed

- **`WriteVideoReordered`: non-deterministic DTS synthesis on jittery
  startups.** Live sessions that begin with a backfill burst (frames
  delivered out of order and/or with duplicated timestamps before the stream
  settles into its regular B-frame GOP) made the v0.0.7 one-shot startup
  probe non-deterministic: depending on the burst's arrival order the probe
  either mis-declared the stream reorder-free after 4 "non-decreasing"
  duplicates (locking DTS==PTS passthrough — duplicate/non-monotonic DTS on
  every B-frame for the rest of the stream, ~80 of 240 frames in field
  captures) or locked a burst-inflated reorder depth (a permanently excessive
  DTS lead). Same input pattern, different session, different result.

  The synthesis is now continuous and never locks a startup measurement:

  - the probe always evaluates its full 8-frame window (no 4-frame monotonic
    early-exit), so the depth measurement is order-tolerant within the
    window — every scramble of a startup burst yields a deterministic, valid
    delay covering the burst's own jitter;
  - passthrough (DTS==PTS) is only declared from a full window with zero
    reordering across at least 4 *distinct* timestamps; a duplicate-heavy
    window (duplicated backfill timestamps carry no ordering evidence)
    extends the probe — bounded at 32 frames — until real ordering evidence
    arrives, instead of mis-locking right before the first B-frames;
  - a reordered duplicate run sizes the delay to at least the run length so
    every duplicate's synthesized DTS stays at or below its PTS;
  - the steady phase keeps measuring reorder depth over a sliding window of
    recent frames: the delay still grows immediately when a deeper B-pyramid
    appears, and now also *decays* (one step per 16-frame evidence window,
    skipping ahead in the sorted-PTS timeline, which preserves monotonicity
    and DTS <= PTS) when a burst-inflated startup measurement exceeds the
    stream's real depth.

  Invariants on the output, verified per frame across 100 scrambled-burst
  orderings (unit + full-session MPEG-TS tests) and externally with
  ffprobe/ffmpeg (0 non-monotonic `pkt_dts_time`, no "non monotonically
  increasing dts" warnings, startup included): DTS strictly monotonic at
  container-clock granularity, DTS <= PTS, decode order preserved, PTS
  deltas unaltered, no frames dropped.

### Changed

- `WriteVideoReordered` startup latency for reorder-free streams is now the
  full 8-frame probe window (was 3 frames when the first 4 arrived
  monotonic); duplicate-only starts may hold up to 32 frames before falling
  back to passthrough. Steady-state latency is unchanged (zero — every
  access unit is still forwarded by the call that delivered it), and
  monotonic/duplicate-burst input remains byte-identical to `WriteVideo`.

## v0.0.7 — 2026-07-27

### Added

- **`Session.WriteVideoReordered(trackID, au, pts)`** — live ingestion for
  video delivered in decode order with per-frame *presentation* timestamps
  only. For H.264/HEVC streams with B-frames, PTS is non-monotonic in decode
  order, so the previous guidance (`WriteVideo`, which sets `DTS = PTS`)
  produced a non-monotonic decode timeline: ffprobe showed ~80 of every 240
  frames with non-increasing `pkt_dts_time`, ffmpeg remuxes warned
  "Application provided invalid, non monotonically increasing dts to muxer",
  and players desynced/dropped frames. The new call synthesizes the DTS:
  - payload (decode) order is preserved exactly; only timestamps are added;
  - the synthesized decode timeline is strictly monotonic, advancing by at
    least `Preprocessor.MinMonotonicStep` so it survives quantization to the
    container clock (90 kHz MPEG-TS);
  - `DTS <= PTS` holds per frame; the caller's PTS values are never altered;
  - **safe to call unconditionally**: input without reordering collapses to
    the `WriteVideo` fast path — `DTS == PTS`, byte-identical output, zero
    steady-state latency — so callers need not know whether a stream carries
    B-frames;
  - added latency is a one-time startup probe of at most 8 frames (3 when
    the first 4 frames arrive monotonic) while the stream's reorder depth is
    measured; afterwards every access unit is forwarded by the call that
    delivered it. If the reorder depth grows mid-stream, the delay adapts
    within a frame or two (bridging frames may transiently carry DTS
    slightly above PTS before the timeline re-converges);
  - `Close` drains frames still held by the startup probe, so streams
    shorter than the probe lose nothing.
- `internal/preprocessor.DTSSynthesizer` implementing the above (sorted-PTS
  delayed by measured reorder depth; bounded probe + adaptive growth capped
  at the H.264 DPB maximum of 16).

### Notes

- `WriteVideo` semantics are unchanged (`pts == dts`); its doc now points
  B-frame decode-order callers at `WriteVideoReordered`.
- Do not mix `WriteVideo`, `WriteVideoReordered`, and `WritePacket` on the
  same track — packets bypassing the synthesizer would fork the decode
  timeline.
- Intended for `ContainerMPEGTS` output (PES carries the distinct PTS/DTS
  pair); the WebM/MKV writer keys blocks on DTS only.

## v0.0.6 — 2026-07-26

- Fixed the Enforcer's equal-DTS insertion reversing duplicate-timestamp
  runs (live backfill bursts lost their leading keyframe to the Aligner).

## v0.0.5 and earlier

- MPEG-TS live output backend (`ContainerMPEGTS`, `WriteVideo`/`WriteADTS`,
  ADTS parsing, `MinMonotonicStep`), MP4 input, MKV output, H.264/HEVC,
  codec Probe API, streaming MP4 parse, WebM/MKV muxing core.
