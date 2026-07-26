# Changelog

All notable changes to puremux are documented here. Versions are git tags on
`main`; the module has no `v1` stability promise yet.

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
