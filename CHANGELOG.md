# Changelog

All notable changes to puremux are documented here. Versions are git tags on
`main`; the module has no `v1` stability promise yet.

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
