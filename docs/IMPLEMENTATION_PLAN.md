# puremux v0.2.0 Implementation Plan

Updated: 2026-09-06
Owner: Codex `/root`

## Release objective

v0.2.0 replaces the duration-based legacy facade with one exact-timestamp media
API and adds production MP4 output. Public packets keep signed integer
PTS/DTS/duration values in their stream time base from demux through mux. The
release writes progressive MP4 to seekable outputs and fragmented MP4 to
non-seekable outputs while retaining pure-Go, non-CGO operation.

## Non-negotiable invariants

1. `CGO_ENABLED=0` builds and tests must pass.
2. No FFmpeg/ffprobe process execution and no pixel or PCM decoding.
3. Demuxers report source timing; muxers serialize ordered timing without
   repairing it. Timestamp repair remains an explicitly selected preprocessor.
4. Compressed payload ownership is explicit. A muxer must not retain caller
   memory after `WritePacket` returns unless it has copied it.
5. Codec header/configuration inspection is isolated in `pkg/bitstream`.
6. Codec and container byte tests use specification-derived values, confirm
   bit order, and cover malformed/truncated/overflow boundaries without panic.

## v0.2.0 public surface

- `pkg/media` is the sole public packet, stream, demuxer, muxer, and remux API.
- `media.Muxer` mirrors `media.Demuxer`: register streams, write exact-tick
  packets, then close/finalize.
- `media.NewMuxer` selects WebM, Matroska, MP4/fMP4, or MPEG-TS from explicit
  options. MP4 auto mode selects progressive for seekable sinks and fMP4 for
  non-seekable sinks.
- `media.Remux` and file helpers replace the legacy `pkg/puremux` facade.
- `cmd/puremux` uses only `pkg/media`.
- The legacy `pkg/puremux` public API is removed without compatibility shims.

## MP4 output scope

### Progressive MP4

- `ftyp`, extended-size `mdat`, and final `moov`.
- `mvhd`, `trak`, `tkhd`, `edts/elst`, `mdia`, `mdhd`, `hdlr`, `minf`, `dinf`.
- `stsd`, run-length `stts`, signed `ctts` v1, `stss`, `stsc`, `stsz`, and
  automatic `stco`/`co64` selection.
- Multi-track interleaving, exact per-track sample durations, signed PTS-DTS,
  deterministic output, checked size/offset arithmetic, and short-write errors.

### Fragmented MP4

- Initialization `ftyp+moov+mvex/trex` followed by `moof+mdat` fragments.
- `mfhd`, per-track `traf/tfhd/tfdt` v1/`trun` v1, exact sample duration/size,
  sync flags, signed composition offsets, and recomputed multi-track offsets.
- Keyframe/GOP cuts for video, duration cuts for audio-only streams, bounded
  fragment bytes/duration, and pooled fragment payload storage.

### Initial codec matrix

| Video | Configuration | Audio | Configuration |
|---|---|---|---|
| H.264/AVC | `avcC` | AAC | ASC in `esds` |
| H.265/HEVC | `hvcC` | Opus | `dOps` / OpusHead conversion |
| VP9 | `vpcC` | FLAC | `dfLa` / STREAMINFO conversion |
| AV1 | `av1C` | | |

VP8, Vorbis, MP3-in-MP4, subtitles, CENC/DRM, manifest generation, and
progressive fast-start relocation are outside v0.2.0.

## Verification gates

- Unit tests for every emitted box and codec configuration record using
  hand-derived ISO BMFF/RFC bytes and boundary cases.
- Writer-to-reader round trips preserving stream metadata, exact tick
  PTS/DTS/duration, keyframe flags, payload bytes, and multi-track order.
- Progressive and fragmented output checked by the independent mp4ff parser in
  tests and with attributed real compressed fixtures.
- WebM/Matroska/MPEG-TS regression tests through the new API.
- Final gates: uncached `CGO_ENABLED=0 go test -count=1 ./...`, `go vet ./...`,
  `go mod tidy -diff`, and `git diff --check`.

## Progress Ledger

| ID | Atomic task | Status | Updated | Assignee | Verification / notes |
|---|---|---|---|---|---|
| 0 | Reset implementation plan for v0.2.0 | Done | 2026-09-04 | Codex `/root` | Replaced the historical ledger with the approved breaking-API and MP4-output release plan. Repository was clean before this documentation-only change. |
| 1 | Define exact public mux/remux contracts in `pkg/media` | Done | 2026-09-04 | Codex `/root` | Added `Muxer`, `MuxOptions`, MP4 layout modes, exact-tick and caller-ownership contracts, and capability validation. Focused non-CGO media tests pass, including nil writer, invalid mode/limits, and progressive-on-nonseekable rejection. |
| 2 | Implement shared ISO BMFF writer primitives and codec sample entries | Done | 2026-09-04 | Codex `/root` | Added checked box/full-box serialization, complete visual/audio sample-entry layouts, AAC ES descriptors, exact output track/sample types, and robust full writes. Verification uses hand-derived big-endian box bytes and MSB-first AAC-LC ASC `12 10`; invalid IDs, scales, dimensions, config types/sizes, and zero-progress writers are rejected. Focused non-CGO MP4 tests pass. |
| 3 | Implement progressive MP4 muxer | Done | 2026-09-04 | Codex `/root` | Added seekable moov-at-end output with 64-bit mdat sizing, v1 movie/media/track times, edit-list track delay, stsd/stts/ctts-v1/stss/stsc/stsz/stco/co64 tables, deterministic multi-track metadata, exact per-track ticks, and idempotent finalization. Writer→existing reader round-trip preserves H.264 config, 1920x1080, DTS 0/3000, PTS 3000/0, duration 3000, sync flags, and bytes; signed -3000 ctts bytes `FF FF F4 48` are asserted. Zero duration, unknown track, DTS gaps, composition overflow, and short writes are rejected. Focused non-CGO MP4 tests pass. |
| 4 | Implement fragmented MP4 muxer | Done | 2026-09-04 | Codex `/root` | Added non-seekable init+fragment output (`mvex/trex`, `mfhd`, default-base-is-moof `tfhd`, 64-bit `tfdt`, signed-v1 `trun`, `mdat`), deterministic track-contiguous payload offsets, keyframe/GOP and audio-duration cuts, byte bounds, pooled fragment buffers, exact duration/flags, and idempotent close. Combined fMP4 round-trip preserves three samples including +3000/-3000 composition offsets across two GOP fragments; exact v1 flags, audio threshold, negative DTS, unknown track, oversize sample, and limit boundaries are tested. Focused non-CGO MP4 tests pass. |
| 5 | Add bounded codec-config validation/conversion | Done | 2026-09-04 | Codex `/root` | Added RFC 7845 OpusHead parsing and endian-correct dOps conversion, ISO BMFF dfLa wrapping/extraction for RFC 9639 STREAMINFO, and bounded validation for avcC, hvcC, ASC, dOps, dfLa, av1C, and vpcC. Validation cross-checks audio rate/channel metadata and rejects missing parameter sets, reserved bits, length overruns, truncated mappings, and malformed records. Verification uses Opus pre-skip 312/input-rate 48000/gain -2 byte derivation, MSB-first AV1/VP9 fields, and exact dfLa FullBox/metadata header bytes; all bitstream and MP4 tests pass with CGO disabled. |
| 6 | Move WebM/Matroska/MPEG-TS writers behind the new mux API | Done | 2026-09-04 | Codex `/root` | Added direct, preprocessor-free adapters for WebM/Matroska and MPEG-TS. EBML supports seekable duration/cues/SeekHead patching and non-seekable unknown-size output; TS converts exact stream ticks without rewriting payloads. Public round trips cover progressive/fMP4 ownership, WebM Opus, and ADTS/AAC TS. RFC 8794 width-8 unknown-size bytes, RFC 6716 Opus TOC duration, cancellation, missing timestamps, invalid kinds/codecs, and idempotent close are covered. Full uncached non-CGO suite passes. |
| 7 | Implement exact multi-input remux and file helpers | Done | 2026-09-04 | Codex `/root` | Added ownership-explicit `RemuxInput`, `Remux`, and atomic `RemuxFiles` with output-extension inference, alias rejection, context propagation, deterministic stream mapping, and exact 192-bit rational DTS comparison (including sub-nanosecond ordering). Primed packets release on every error path. Added framing compatibility rejection for TS. WebM now emits per-packet BlockGroup/BlockDuration, ReferenceBlock, and signed DiscardPadding so non-self-describing codec duration survives remux. Hand-derived MSB-first EBML bytes cover timecode -2, duration 40, reference -1, padding -128, invalid tracks/zero duration/closed writer. WebM Opus and Matroska H.264 to MP4 plus replacement-file round trips pass; full uncached non-CGO suite passes. |
| 8 | Remove legacy `pkg/puremux` API and migrate CLI | Done | 2026-09-04 | Codex `/root` | Removed the complete legacy facade/session/merge/probe package and its compatibility tests. CLI now imports only `pkg/media`, exposes `probe` and `remux`, infers MP4/WebM/Matroska/TS output by extension, propagates cancellation before filesystem access, and advertises MP4 output. The HLS cross-track fixture now uses the exact mux API. CLI probe/remux/cancellation tests and the full uncached non-CGO suite pass. Deleted sources remain recoverable from Git history. |
| 9 | Add real-fixture interoperability and boundary verification | Done | 2026-09-04 | Codex `/root` | Added an attributed 184-byte real H.264 Annex-B black-frame fixture from Eyevinn/mp4ff commit `ec2f82c` (MIT) and test-only mp4ff v0.56.0 oracle. Fixture NAL boundaries/types are checked before building avcC and length-prefixed sample bytes. Independent parsing validates progressive ftyp/moov/extended-mdat, avc1/avcC, stsz/ctts-v1, chunk offset and byte-identical sample extraction; fragmented parsing validates init/mvex, moof/traf/trun/mdat, data offset, DTS/PTS/duration, and byte-identical sample extraction. Empty/header/truncated tails must not appear structurally complete. Full uncached non-CGO suite passes. |
| 10 | Update architecture, changelog, and v0.2.0 migration guide | Done | 2026-09-04 | Codex `/root` | Replaced the obsolete Session-era architecture with the v0.2 exact-tick media layers, ownership rules, MP4 layouts/codecs, EBML duration semantics, TS framing boundaries, exact remux algorithm, and release gates. Added `MIGRATION_v0.2.0.md` with file/direct mux/probe/CLI examples and explicit framing/ownership changes. Added the v0.2.0 changelog. Documentation review exposed and fixed TS accepting zero duration; the boundary test confirms rejection before output. Focused non-CGO media, CLI, MP4, and WebM tests pass. |
| 11 | Run final non-CGO, vet, tidy, and diff gates | Done | 2026-09-04 | Codex `/root` | Final audit added checked int64 timestamp arithmetic, extreme-value rejection before output, overflow-safe duration differences, and corrected progressive `mdhd` decode duration with an mp4ff assertion. Final `CGO_ENABLED=0 go test -count=1 ./...`, `go vet ./...`, `go mod tidy -diff`, and `git diff --check` all pass. Git only reports the repository's informational LF-to-CRLF checkout warnings. |
| 12 | Independently re-verify the complete current implementation | Done | 2026-09-04 | Codex `/root` | Verification completed with release-blocking findings; no production code was changed in this verification-only task. Native Windows/amd64 non-CGO tests (uncached, shuffled, and shuffled x10), vet, tidy diff, module verification, Windows/Linux/Darwin non-CGO builds, and diff checks pass; statement coverage is 72.2%. Official RFC 9639, RFC 7845, ISO AVC configuration syntax, and Matroska element/codec mappings were re-derived against the implementation. Findings include a non-standard Matroska HEVC CodecID, missing mandatory Opus initialization metadata, invalid fixed `ReferenceBlock=-1` semantics, incomplete AVC/HEVC/Opus/FLAC validators, and Windows/386 compile/panic boundaries. Race instrumentation is unverified because the Go 1.27 Windows race/cgo frontend exits before package compilation. |
| 13 | Correct Matroska codec and dependency metadata interoperability | Done | 2026-09-04 | Codex `/root` | Corrected HEVC to the registered `V_MPEGH/ISO/HEVC` CodecID, required and parsed RFC 7845 OpusHead, cross-checked channels, emitted the input sample rate, derived CodecDelay from the spec byte sequence pre-skip `38 01` (312 samples = 6,500,000 ns), defaulted SeekPreRoll to 80,000,000 ns, and encoded unknown non-keyframe dependency as permitted `ReferenceBlock=0`. Exact EBML bytes, truncated/missing OpusHead, channel/delay mismatch, negative timing, round-trip metadata, and endian-correct 48 kHz bytes `80 BB 00 00` are covered. Focused WebM and public media tests pass with CGO disabled. |
| 14 | Close FLAC, Opus, AVC, and HEVC validation gaps | Done | 2026-09-04 | Codex `/root` | Enforced RFC 9639 STREAMINFO block-size 16..65535 and 4..32-bit sample bounds and rejected frame block code 7's forbidden stored `FF FF` (65536); validated RFC 7845 channel indices against N+M in both little-endian OpusHead and big-endian dOps; enforced AVC and HEVC reserved bits; and required type-correct AVC SPS/PPS plus HEVC VPS/SPS/PPS NAL arrays. Tests use hand-derived MSB-first configuration bytes, explicit NAL header types, truncated/overrun/trailing/forbidden/reserved cases, and matching normal records. All affected bitstream, MP4, and public media tests pass with CGO disabled. |
| 15 | Make all audited length paths safe on 32-bit targets | Done | 2026-09-04 | Codex `/root` | Replaced architecture-dependent int comparisons and pre-validation conversions in NAL framing, MP4 sample limits, AVCC keyframe walking, Ogg OpusTags, raw Vorbis comments, and ID3v2 frames with unsigned bounds checked against remaining buffers before conversion/addition. Ogg Opus mapping now also applies RFC 7845 N+M index rules. Regression inputs include `7F FF FF FF` and `FF FF FF FF` lengths, nil/truncated data, oversized vendor/comments, and forbidden mappings; affected amd64 tests and the complete uncached Windows/386 non-CGO suite pass without panic. |
| 16 | Complete MP4 codec-entry coverage, parser fuzzing, and formatting | Done | 2026-09-04 | Codex `/root` | Added direct sample-entry coverage for AVC, HEVC, AV1, VP9, AAC, Opus, and FLAC, asserting registered entry/config box types from hand-derived avcC/hvcC/av1C/vpcC/ASC/dOps/dfLa records plus malformed AV1 reserved bits and VP9 lengths. Added fuzz entry points for every bitstream package and the high-risk EBML, Ogg Opus, and Vorbis-comment parsers; each target completed a one-second mutation run with no panic. Formatted every Go source (`gofmt -l .` empty), and all affected non-CGO tests plus `git diff --check` pass. |
| 17 | Remove the final stale HEVC mapping reference | Done | 2026-09-04 | Codex `/root` | Updated the core codec comment from the obsolete `V_MPEGH/ISO/SHEVC` spelling to the registered `V_MPEGH/ISO/HEVC`; a repository-wide search confirms the obsolete spelling remains only in historical ledger entries that document the original defect. |
| 18 | Normalize the remaining WebM import groups | Done | 2026-09-04 | Codex `/root` | Grouped standard-library and project imports consistently in the remaining three WebM files after the repository-wide formatting pass. `gofmt -l .` is empty and focused WebM tests pass with CGO disabled. |
| 19 | Re-run complete release verification after audit fixes | Done | 2026-09-04 | Codex `/root` | Final uncached `CGO_ENABLED=0 go test -count=1 ./...`, shuffled x10 suite, Windows/386 suite, vet, tidy diff, module verification, format, banned-import, and whitespace gates pass. Linux amd64/arm64, Darwin amd64/arm64, and Windows amd64/arm64 non-CGO builds pass; total statement coverage is 72.8%. All ten fuzz targets completed mutation runs without panic. Windows race instrumentation remains unavailable outside the repository: Go 1.27.0's `cgo.exe` exits with status 2 while building the standard `runtime/cgo` package itself, with both the full repository and a single dependency-free package; WSL is not installed. This is not a puremux compile or test failure. |
| 20 | Verify the full race suite in MSYS2 UCRT64 | Done | 2026-09-05 | Codex `/root` | Ran Windows Go 1.27.0 from an MSYS2 UCRT64 login shell with `/ucrt64/bin/gcc` 16.1.0 and the cache under `F:/cache`. `CGO_ENABLED=1 go test -race -count=1 ./...` passes for every package. This closes the environment-only race-verification gap recorded in task 19; no production code changed. |
| 21 | Re-verify all current implementations and specification boundaries | Done | 2026-09-05 | Codex `/root` | Verification-only audit; no production code changed. In MSYS2 UCRT64 with the cache under `F:/cache`, uncached non-CGO tests, shuffled x10 tests, Windows/386 tests, the full race suite, vet, tidy diff, module verification, six non-CGO OS/architecture builds, formatting, banned-dependency, whitespace, real-fixture/mp4ff interoperability, and `govulncheck` all pass; statement coverage is 72.8%. All 10 fuzz targets completed 1,003,451 mutation executions without panic. Manual specification re-derivation confirmed MSB-first AAC/AV1/VP9/FLAC/ISO-BMFF fields, big-endian NAL/EBML/TS fields, little-endian OpusHead fields, and the existing truncated/overrun/forbidden-bit cases. Release remains blocked by three newly confirmed gaps: EBML muxing accepts missing or malformed mandatory CodecPrivate for AV1, AVC, HEVC, FLAC, and Vorbis; the HEVC keyframe/config-only detector accepts the forbidden `nuh_temporal_id_plus1=0`; and Matroska block timestamp addition can wrap at the int64 boundary when TimestampScale is 1 ns. |
| 22 | Reject invalid HEVC temporal identifiers in detectors | Done | 2026-09-05 | Codex `/root` | Centralized HEVC NAL-header validity, rejecting the forbidden `nuh_temporal_id_plus1=0` in both keyframe and configuration-only detection and requiring IRAP keyframes to encode TemporalId 0. Verification uses hand-derived MSB-first header bytes `28 00`, `28 02`, and `28 01`, plus nil/truncated and forbidden-bit coverage; focused non-CGO core tests pass in MSYS2 UCRT64. |
| 23 | Prevent Matroska block timestamp addition overflow | Done | 2026-09-05 | Codex `/root` | Added a checked addition before combining the unsigned cluster timestamp with a positive signed Block relative timecode, preventing int64 wrap before timestamp scaling. Verification uses the RFC 9559-derived big-endian Block bytes `81 00 01 80 00`, rejects `MaxInt64 + 1` without queuing output, accepts the `MaxInt64-1` timestamp plus one-tick duration boundary, and retains existing nil/truncated/malformed parser coverage; focused non-CGO WebM tests pass in MSYS2 UCRT64. |
| 24 | Enforce mandatory Matroska and WebM codec initialization | Done | 2026-09-05 | Codex `/root` | Added format-specific validation for mandatory AVC avcC, HEVC hvcC, AV1 av1C, FLAC native initialization, and Xiph-laced Vorbis CodecPrivate; introduced a Vorbis-header config tag so demux/remux preserves its representation. Raw 34-byte FLAC STREAMINFO is validated against stream properties and normalized to the `fLaC` marker plus final STREAMINFO block. Verification uses hand-derived MSB-first AVC/HEVC/AV1/FLAC bytes, little-endian Vorbis identification/comment fields, Xiph lacing, valid public Vorbis write/read round-trip, and missing, nil, truncated, length-overrun, property-mismatch, reserved-bit, forbidden temporal-ID, and wrong-NAL-type cases; focused non-CGO media tests pass in MSYS2 UCRT64. |
| 25 | Isolate shared codec-configuration validation | Done | 2026-09-05 | Codex `/root` | Moved AVC, HEVC, AV1, FLAC, and Vorbis configuration inspection behind `pkg/bitstream` APIs and made MP4 and EBML reuse the applicable validators, restoring the architecture boundary that container code only dispatches and maps errors. Added isolated AV1 and Vorbis suites using spec-derived MSB-first AV1 bytes, little-endian Vorbis fields, Xiph lacing, and nil/truncated/reserved/framing/length-overrun/property-mismatch boundaries. All bitstream packages plus focused MP4 and media tests pass with CGO disabled in MSYS2 UCRT64. |
| 26 | Fuzz new AV1 and Vorbis configuration validators | Done | 2026-09-05 | Codex `/root` | Added native fuzz entry points seeded with the spec-derived valid and truncated/overrun AV1 and Xiph-laced Vorbis records. Three-second non-CGO mutation runs in MSYS2 UCRT64 completed 728,119 AV1 and 630,792 Vorbis executions without panic; focused deterministic suites also pass. |
| 27 | Preserve complete Matroska FLAC initialization chains | Done | 2026-09-05 | Codex `/root` | Extended FLAC CodecPrivate validation from the minimal final STREAMINFO form to the complete pre-frame metadata chain required by the mapping, while preserving every valid native block byte. Verification uses RFC 9639-derived MSB-first 24-bit lengths and last/type bits for a non-final 34-byte STREAMINFO plus final zero-length padding block; nil, truncated header, size overrun, duplicate STREAMINFO, missing-final, and data-after-final cases are rejected without panic. Focused non-CGO FLAC and public media tests pass in MSYS2 UCRT64. |
| 28 | Fuzz Matroska FLAC metadata-chain validation | Done | 2026-09-05 | Codex `/root` | Extended the existing FLAC fuzz target with raw and native `fLaC` initialization seeds and the metadata-chain validator. A three-second non-CGO run in MSYS2 UCRT64 completed 1,469,423 executions without panic, including 19 newly interesting paths. |
| 29 | Validate AVC High Profile configuration extensions | Done | 2026-09-05 | Codex `/root` | Corrected a false-pass test record that declared High Profile without its conditional avcC extension. The parser now validates the required MSB-first reserved/chroma/bit-depth fields, SPS-extension array bounds and trailing data; the shared validator additionally enforces SPS-extension NAL type 13. Minimum container tests now use Baseline profile, while the real High Profile fixture emits `FD F8 F8 00` (4:2:0, 8-bit, zero SPS extensions). Missing extension, malformed reserved fields, wrong type, truncated/overrun, and trailing bytes are covered. Focused H.264, MP4/mp4ff interoperability, and media tests pass; a three-second fuzz run completed 1,490,115 executions without panic. |
| 30 | Re-run final release gates after all audit fixes | Done | 2026-09-05 | Codex `/root` | No release blocker remains from task 21. In MSYS2 UCRT64 with caches under `F:/cache`, final uncached `CGO_ENABLED=0` tests with coverage, shuffled tests x10, Windows/386 tests, and the complete race suite pass. Vet, module verification, tidy diff, gofmt, banned production `C`/`os/exec` imports, whitespace, mp4ff real-fixture interoperability, and Linux/Darwin/Windows amd64+arm64 non-CGO builds all pass. Total statement coverage is 73.1%; targeted AV1, Vorbis, FLAC, and AVC fuzz runs completed 4,318,449 mutations without panic. |
| 31 | Pre-tag v0.2.0 conformance audit | Done | 2026-09-05 | Codex `/root` | Verification-only audit; no production code changed. Current-tree gates pass: uncached `CGO_ENABLED=0` tests with 73.1% coverage, vet, gofmt, tidy diff, module verification, whitespace and banned-dependency checks, shuffled x10 and Windows/386 tests, race, six-target non-CGO cross-builds, focused interoperability/round-trip suites, govulncheck, and all 12 fuzz targets (1,593,009 mutation executions, no panic). Official specification re-derivation nevertheless found release blockers: AV1 MP4 branding/config reserved-bit/configOBU/colr conformance gaps; VP9 MP4 vpcC versus Matroska CodecPrivate feature-list conflation plus incomplete vpcC semantic validation; failure to normalize valid full Matroska FLAC metadata chains to MP4 while accepting a truncated non-final minimal chain; and omission of mandatory Opus CodecDelay when pre-skip is zero. The working tree is also uncommitted (`v0.1.1-dirty`), so tagging HEAD would not include v0.2.0 changes. Verdict: do not tag v0.2.0 until these blockers are fixed and the release gates are rerun. |
| 32 | Enforce AV1 MP4 configuration and signalling conformance | Done | 2026-09-05 | Codex `/root` | Validated the conditional av1C delay-reserved nibble, profile/level bounds, low-overhead OBU forbidden/reserved/extension bits, mandatory size flags, LEB128 truncation, payload overruns, reserved OBU types, and Sequence Header ordering/count. AV1 MP4 output now advertises the mandatory `av01` compatible brand and emits `colr/nclx` with unspecified 2/2/2 CICP values and limited range when configOBUs has no Sequence Header. Verification uses an AV1-spec-derived MSB-first reduced-still-picture Sequence Header (`18 00 00 11`), matching av1C byte `1C`, a valid zero-length Temporal Delimiter OBU (`12 00`), exact nclx bytes, nil/truncated/overrun/forbidden/reserved cases, and passing focused non-CGO AV1, MP4, and media suites in MSYS2 UCRT64. |
| 33 | Separate and convert VP9 MP4 and Matroska configurations | Done | 2026-09-05 | Codex `/root` | Added an explicit `CodecConfigVP9FeatureMetadata` representation and isolated VP9 validators/converters in `pkg/bitstream/vp9`. MP4 vpcC now converts to Matroska ID/length/value feature metadata, Matroska demux identifies that representation correctly, and complete feature metadata converts back to vpcC with registered default colour values. vpcC validation now enforces FullBox v1 flags, profile/level/bit-depth/chroma ranges and profile combinations, identity-matrix chroma, and the mandatory zero initialization-data size; feature metadata validates reserved ID bits, IDs, lengths, duplicates, truncation, and value combinations. Verification uses WebM/VP9-spec-derived exact TLVs (`01 01 00 02 01 0A 03 01 08 04 01 01`) and MSB-first vpcC packing (`82` for 8-bit/chroma-1/limited), malformed boundaries, a seeded fuzz target, and passing focused non-CGO VP9, MP4, WebM, and media suites in MSYS2 UCRT64. |
| 34 | Normalize complete Matroska FLAC initialization to MP4 | Done | 2026-09-05 | Codex `/root` | Added a validated Matroska CodecPrivate-to-dfLa conversion that accepts raw STREAMINFO or the complete native pre-frame metadata chain, extracts only the mandatory first STREAMINFO body, and emits the canonical version-0/final-STREAMINFO dfLa payload. MP4-origin dfLa is now explicitly validated instead of accepting arbitrary 42-byte data, and a native minimal record whose STREAMINFO last-block flag is zero is rejected. Verification uses RFC 9639-derived big-endian headers: non-final STREAMINFO `00 00 00 22`, final zero-length padding `81 00 00 00`, and canonical dfLa `00 00 00 00 80 00 00 22`; nil, truncation, size overrun, duplicate STREAMINFO, missing-final, and data-after-final boundaries are covered. Focused non-CGO FLAC, MP4, and media tests pass in MSYS2 UCRT64. |
| 35 | Emit mandatory zero-valued Opus CodecDelay | Done | 2026-09-05 | Codex `/root` | WebM/Matroska track serialization now emits CodecDelay for every Opus track, including legal OpusHead pre-skip zero, while retaining optional nonzero-only behavior for other codecs. Verification uses an RFC 7845-derived little-endian OpusHead with pre-skip `00 00`, input rate `80 BB 00 00`, gain zero, and mapping family zero; the required zero-valued EBML element is asserted as exact bytes `56 AA 81 00`. Both isolated WebM serialization and public mux integration tests pass with CGO disabled in MSYS2 UCRT64. |
| 36 | Re-run v0.2.0 release gates after conformance fixes | Done | 2026-09-05 | Codex `/root` | All four task-31 code blockers are closed and CHANGELOG/architecture documentation is synchronized. In MSYS2 UCRT64 with caches under `F:/cache`, uncached `CGO_ENABLED=0` tests pass at 73.5% total statement coverage; vet, gofmt, tidy diff, module verification, shuffled x10 tests, Windows/386 tests, the complete race suite, focused mp4ff interoperability/public round-trip tests, Linux/Darwin/Windows amd64+arm64 non-CGO builds, govulncheck, banned production dependency scan, and git diff whitespace checks all pass. All 13 fuzz targets completed 3,574,487 mutation executions without panic. No code/specification blocker from the final audit remains; the tree is intentionally still `v0.1.1-dirty`, so the v0.2.0 changes must be committed before tagging. |
| 37 | Cover the complete reserved AV1 profile range | Done | 2026-09-05 | Codex `/root` | Final diff review found that the new av1C profile bound rejected reserved profile 3 but not reserved profiles 4 through 7. Changed the condition to reject every `seq_profile > 2` and added a profile-4 byte case (`80`, MSB-first). Focused AV1/MP4/media tests and the complete uncached non-CGO suite, vet, and gofmt pass in MSYS2 UCRT64 after the correction. This does not reopen any task-31 blocker. |
| 38 | Perform final v0.2.0 pre-version verification | Done | 2026-09-05 | Codex `/root` | Verification-only audit; no production code changed. In MSYS2 UCRT64 with caches under `F:/cache`, uncached `CGO_ENABLED=0` tests pass at 73.5% total statement coverage; shuffled x10 and Windows/386 tests, the complete race suite, focused mp4ff/round-trip/remux tests, vet, tidy diff, module verification, gofmt, six Linux/Darwin/Windows amd64+arm64 non-CGO builds, govulncheck v1.7.0, banned-dependency/legacy-import/conflict-marker/TODO scans, and whitespace checks all pass. All 13 fuzz targets completed 14,248,178 executions without panic. Code gates are clear, but release tagging remains procedurally blocked because `HEAD` is still tagged `v0.1.1` and the complete v0.2.0 tree is uncommitted (`v0.1.1-dirty`). |
| 39 | Align v0.2.0 release metadata for publication | Done | 2026-09-05 | Codex `/root` | Updated the changelog release date to 2026-09-05 after the complete task-38 release matrix passed. Fetched `origin` and confirmed `main` is synchronized (0 ahead/0 behind before the release commit) and `v0.2.0` is absent both locally and remotely. The repository is ready for the authorized release commit, annotated tag, and push. |
| 40 | Correct the release designation to v0.2.0 | Done | 2026-09-05 | Codex `/root` | Discarded the superseded local release commit with a soft reset to `v0.1.1`, preserving the complete verified implementation, and removed its local annotated tag. Replaced every release reference with v0.2.0, renamed the migration guide to `MIGRATION_v0.2.0.md`, and confirmed a repository-wide stale-version scan is empty. Product code is unchanged from the task-38 verification; the corrected release commit and atomic remote ref replacement are authorized. |
| 41 | Add a generic opt-in live normalization layer | Done | 2026-09-05 | Codex `/root` | `media.LiveMuxer` provides bounded generic packet normalization plus optional Annex-B POC and ADTS helpers. Spec-derived H.264/ADTS and boundary tests cover fractional clocks, discontinuities, A/V bounds, ownership, and malformed input. The final MSYS2 UCRT64 release matrix passes at 74.0% coverage, and `$code-review` reports no remaining findings. |
| 42 | Audit edge cases, concurrency, interoperability, and performance | Done (audit; fixes open) | 2026-09-05 | Codex `/root` | Added `docs/AUDIT_2026-09-05.md`. Existing uncached non-CGO tests, vet, and full race suite pass in MSYS2 UCRT64. Fifteen isolated overlay tests under `F:/cache/puremux-audit-20260905` fail as expected and demonstrate 13 root causes: manifest recovery/EOF, DASH Close deadlock, live-buffer starvation, Ogg EOS timing, MP4 edit/DTS, empty stss, TS video remux duration, reverse codec-config conversion, manifest seek origin, exact seek ticks, redirect base, edit rescale overflow, and eager mdat reads. Verification uses existing spec-derived fixtures, MSB-first AAC/config fields, big-endian empty stss, and RFC 7845-derived little-endian EOS granules; no payload decoding or production-code changes. Resource-bound, metadata, support-scope, and optimization findings are explicitly distinguished from runtime reproductions. Audit complete; findings remain unfixed. |
| 43 | Repair manifest recovery, cancellation, and redirect base | Done | 2026-09-05 | Codex `/root` | Focused HLS/DASH suites and audit recovery/EOF/deadlock/redirect regressions pass with CGO disabled. EOF transitions remain retryable; Close cancels nested reads; bounded fetch uses final retrieval URI. Seek translation uses read shift; exact MP4 seek verified next. |
| 44 | Preserve exact MP4 clocks, sync tables, and lazy payload access | Done | 2026-09-05 | Codex `/root` | Exact track-tick seek and rational comparisons avoid nanosecond loss; edit shifts apply to DTS/PTS with checked rescaling; empty stss remains distinct from absence. Bounded top-level parsing seeks over mdat and sorts fragments once. Focused progressive/fragmented/media suites and audit seek/origin/delay/overflow/stss/mdat tests pass. Big-endian ISO BMFF fields and truncation/overflow boundaries confirmed; opaque payloads unchanged. |
| 45 | Normalize reverse Opus and FLAC container configurations | Done | 2026-09-05 | Codex `/root` | Added validated dOps-to-OpusHead conversion and dfLa-to-Matroska normalization. Focused Opus/FLAC/media suites and both audit conversion regressions pass. RFC7845 little-endian Head versus big-endian dOps fields and signed gain are round-trip checked; nil/truncated/invalid-version and existing reserved/mapping boundaries covered. |
| 46 | Guarantee progress when live jitter buffers fill | Done | 2026-09-05 | Codex `/root` | Capacity overflow now emits the earliest ordered packet instead of discarding it before its time window matures. Enforcer/live suites and 100-packet audit starvation regression pass; ordered packets are retained and the existing zero-window and timestamp-saturation boundaries remain covered. |
| 47 | Preserve Ogg EOS packet starts and expose tail trimming | Done | 2026-09-05 | Codex `/root` | EOS timing follows previous completed granule or the RFC7845 initial-EOS rule; public packets expose DiscardPadding. Removed redundant adapter payload copy and bounded continued-packet retention to 16 MiB. Ogg/media suites and spec-derived little-endian 960/1440 granule regression pass; existing malformed/truncated/CRC and mapping boundaries pass. |
| 48 | Repair TS video remux and bound transport ingestion | Done | 2026-09-05 | Codex `/root` | Unknown video duration stays unknown and TS serialization permits it because PES has no duration field; other muxers retain strict duration checks. Indexed TS reuses incremental parsing, avoiding full-file and completed-PES duplicate copies, with bounded packet/byte retention. Streaming startup rejects EOF before track readiness; detector dispatch shares core validation. Focused TS/PES/stream/remux suites and TS identity audit regression pass with CGO disabled; existing MSB-first PES timestamp and malformed boundary bytes retained. |
| 49 | Close remaining manifest and output failure paths | Done | 2026-09-05 | Codex `/root` | Bound active HLS init cache; checked seek subtraction; recognize terminal manifests without new segments. Fault injection verifies failed output rollback preserves original backup and reports both errors/path. MP4 rejects unsupported discard padding rather than silently dropping it. go test ./pkg/media passes. |
| 50 | Bound fragmented writer metadata and remove repeated payload copying | Done | 2026-09-05 | Codex `/root` | Incremental fragment duration; 65,536-packet cap; direct mdat header/payload writes; discard pooled buffers larger than 1 MiB. Tiny-packet cap roundtrip, existing independent parser/GOP/overflow tests pass. ASC spec-derived MSB-first; MP4 header size big-endian; packet-count boundary verified. |
| 51 | Coalesce sequential HTTP reads with bounded read-ahead | Done | 2026-09-05 | Codex `/root` | 32 KiB sequential cache; ReaderAt remains independent; explicit Seek revalidates validators. 1,000 single-byte reads require probe + one data request. Source mutation and closed-cache reads rejected; full pkg/media tests pass. |
| 52 | Preserve supported track metadata and make unsupported loss explicit | Done | 2026-09-05 | Codex `/root` | MP4 ISO-639-2 language; EBML language/name/default flag roundtrip. MP4/EBML unsupported stream metadata and Remux container tags require explicit AllowMetadataLoss. Discard-padding rejection tested. Spec-derived 5-bit MSB-first kor=0x2df2 verified; backend tests pass. |
| 53 | Eliminate redundant progressive MP4 seek passes | Done | 2026-09-05 | Codex `/root` | Choose requested/fallback samples in one scan and retain chosen cursor state, avoiding rebuild to chosen index. Exact-tick, B-frame, backward/keyframe, empty-stss regression tests pass. Cursor timestamp overflow now propagates corruption. |
| 54 | Verify aggregate parser limits and shared TS NAL validation | Done | 2026-09-05 | Codex `/root` | TS pending count/byte limits exercised without OOM; HEVC MSB-first type20 header 28 01 accepted, forbidden 28 00/a8 01 and truncated/nil rejected. Ogg continued packet exact16MiB accepted, oversize/truncated rejected; unsigned lacing and continued flag derived from Ogg layout. Focused suites pass. |
| 55 | Align public output contracts and fuzz coverage with fixes | Done | 2026-09-05 | Codex `/root` | TS rejects unsupported stream metadata unless explicitly allowed and rejects discard padding; audio default semantics retained. Architecture documents unknown TS duration exception, enforcer comments reflect pressure emission. Added inverse Opus conversion to fuzz target. Focused tests pass. |
| 56 | Verify integrated audit fixes | Done | 2026-09-05 | Codex `/root` | CGO_ENABLED=0 go test -count=1 ./..., go vet ./..., go build ./...; CGO_ENABLED=1 go test -race -count=1 ./... all pass. Opus inverse/config fuzz 10s, 1,274,505 executions passes. gofmt and git diff --check clean. Original 15 top-level audit regressions now pass. |
| 57 | Publish audit fix matrix and compatibility limits | Done | 2026-09-05 | Codex `/root` | Added AUDIT_FIXES_2026-09-05.md; original audit clearly marked historical. Documented all 13 runtime fixes, bounds/performance changes, explicit metadata/trim errors, full verification, and unchanged product scope plus linear seek/in-memory TS limits. Documentation diff check passes. |
| 58 | Prepare v0.2.2 release and verify publication candidate | Done | 2026-09-05 | Codex `/root` | User authorized release through remote. Added v0.2.2 changelog with explicit metadata/discard-padding behavior changes and resource limits; corrected fragment memory contract. Fresh uncached non-CGO tests, vet/build, tidy -diff, module verification and diff checks pass; previous integrated race/fuzz results remain valid (no product edits since). Authenticated HTTPS fetch confirms remote main matches v0.2.1 parent and v0.2.2 is absent. Publication uses main plus annotated v0.2.2 tag, following existing tag-only releases. |
| 59 | Open WebM incrementally without whole-file indexing or SeekEnd | Done | 2026-09-05 | Codex `/root` | Stop startup at first Cluster after Info/Tracks; collect clusters/Cues/Tags during playback; defer complete unordered/track-specific seek index to explicit Seek with cursor rollback on failure. Optional known byte size avoids SeekEnd; unknown length discovered by reads. Spec-derived MSB-first VINT/BE timestamp fixture and independent libwebm golden both yield first packet with tail unavailable; known/unknown sizes, failed seek resume and deduplicated retry pass. Focused webm/media tests pass. |
| 60 | Verify public spool startup and deferred WebM seek/metadata boundaries | Done | 2026-09-05 | Codex `/root` | Public ContextSource Open + first packet succeeds on unchanged libwebm golden with SeekEnd and tail access forbidden. Internal tests cover trailing Cues/Tags collection, immediate track-specific forward/backward cue seek, failed index preserving pending fixed-size lacing, cancellation, nil/truncated headers and truncated block consumption. RFC8794 MSB-first VINTs, RFC9559 BE timestamps/relative offsets and fixed-lacing bytes hand-derived; focused suites pass. |
| 61 | Preserve lazy WebM metadata visibility and Remux loss checks | Done | 2026-09-05 | Codex `/root` | Info exposes tags collected during playback. Remux rechecks metadata after draining lazy inputs so trailing tags still require explicit AllowMetadataLoss; RemuxFiles retains failure-before-install semantics. Spec-derived RFC9559 Tags/Tag/SimpleTag bytes and MSB-first lengths verify delayed visibility, default rejection and opt-in success. Focused media/WebM tests pass. |
| 62 | Verify and document deferred WebM initialization fix | Done | 2026-09-05 | Codex `/root` | Fresh full uncached CGO_ENABLED=0 tests, vet/build, complete CGO_ENABLED=1 race suite and diff check pass. Startup regression accesses 76/1,048,672 bytes before first packet with both known/unknown length. Independent libwebm/public ContextSource coverage passes. Architecture and Unreleased changelog document prefix startup, deferred validation/tags, required metadata layout and explicit Seek potentially waiting for the tail. User's original video/Discord output not exercised; changes remain local and unreleased. |
| 63 | Audit puremux integration and reusable playback features for lavalink-go | Done | 2026-09-05 | Codex `/root` | Read consumer audio/player/spool paths and instructions. Existing non-CGO audio/httpproxy/player tests pass with released v0.2.2 and temporary modfile pointing at local puremux. Four cache-only overlay tests intentionally fail: HTTP32KiB read-ahead waits past available prefix, fMP4/Ogg eager tail access, consumer Opus140ms accepted. Spec-derived AAC/Opus/Ogg byte values and bit order documented. Added integration audit with 12 ranked findings, explicit static/runtime distinction and generic feature ownership. No product code or consumer module files changed; Discord/CGO end-to-end not tested. |
| 64 | Add HTTP low-latency selection and share Opus timing/config validation | Done | 2026-09-05 | Codex `/root` | ReadAheadBytes=-1 bypasses prefetch; consumer HTTP source selects it. Exposed opus.PacketSamples/PacketDuration and delegated core/consumer TOC parsers; consumer dOps uses shared HeadFromDOPS. Prefix HTTP and forbidden140ms regressions now pass, RFC6716 MSB-first TOC/count and120ms cap confirmed. Focused library tests and consumer audio/player via local modfile pass. |
| 65 | Remove impossible DASH duration bound check (SA4003) | Done | 2026-09-05 | Codex `/root` | time.Duration already has int64 range; removed always-false comparison and unused math import. CGO_ENABLED=0 focused DASH/segment timestamp tests pass. |
| 66 | Preserve initialization transport and cancellation error chains | Done | 2026-09-05 | Codex `/root` | Open wraps ErrInvalidData and underlying causes with %w; WebM EBML/Segment reads retain transport errors. Table tests cover cancellation, deadlines and transport failures across probing, WebM, Ogg and MP4. Full media and WebM suites pass with CGO_ENABLED=0. |
| 67 | Open Ogg from headers and discover duration during playback | Done | 2026-09-05 | Codex `/root` | Public Open uses header-only initialization without SeekEnd; playback validates page sequence/CRC and discovers EOS duration. Explicit seek builds a temporary index and restores playback on failure. RFC7845 little-endian OpusHead/granules and RFC6716 F8 20ms packets verify gated-prefix startup, failed-index cursor restoration and late 40ms duration; existing malformed/truncated/CRC boundary suites plus full media/Ogg tests pass (CGO_ENABLED=0). |
| 68 | Read fragmented MP4 one movie fragment at a time | Done | 2026-09-05 | Codex `/root` | Public Open stops after first moof; NextSample discovers subsequent fragments. Failed full-index seeks preserve playback state; partial moof parsing rolls back appended samples. Gated-download regression verifies first packet before tail, failed seek and second-fragment PTS. Full media/MP4 suites pass with CGO disabled, including independent mp4ff structural validation, big-endian ISO BMFF fixture and malformed/truncated boundary tests. |
| 69 | Propagate consumer seek cancellation, live waits and packet timing | Done | 2026-09-05 | Codex `/root` | lavalink-go seek requests carry the six-second context through demux I/O; fresh playback context survives failed seek; timestamp arithmetic is bounded. Live ErrNoNewSegments waits interruptibly instead of reopening. DurationKnown/Info and metadata refresh dynamically; Opus gaps use previous actual packet duration. Consumer audio/player suites pass using local replacement, including timed seek cancellation, live wait/cancel, late metadata and contiguous 2.5–120ms Opus timing tests. |
| 70 | Retain consumer packet trim and gate Opus passthrough on configuration | Done | 2026-09-05 | Codex `/root` | Consumer carries DiscardPadding to FFmpeg AV_PKT_DATA_SKIP_SAMPLES (header-derived u32 little-endian start/end plus reasons). Actual libavcodec decoding verifies 960-sample silence becomes 480 samples for either 10ms front or end trim. Passthrough requires stereo 48kHz, mapping 0, zero gain/pre-skip/CodecDelay and no trim; no-CGO EOF regression uses explicitly untrimmed RFC fixtures. Consumer audio/player CGO=0 and audio CGO=1 suites pass. CGO player build is externally blocked by absent dave.pc; no Discord audibility claim. |
| 71 | Separate consumer audible seek target from decoder preroll | Done | 2026-09-06 | Codex `/root` | Selected-audio time bases and backward seeks work across puremux, cache and FFmpeg; cached seeks honor direction, retain previous cursor on failure and expose nonzero origin. Preroll becomes bounded per-packet skip metadata, initial Opus pre-skip resets after seek, and packet timestamp rescale overflow is rejected. Consumer CGO=0 audio/player and CGO=1 audio suites pass, including selected 48kHz track request at 420ms for 500ms audible target, native FFmpeg seek tests and combined preroll/end-trim bounds. |
| 72 | Support unknown-length range-free HTTP MPEG-TS input | Done | 2026-09-06 | Codex `/root` | Added caller-owned-client HTTPStreamSource with terminal read cancellation and explicit sequential probing option; existing no-consumption default without FormatHint remains intact. Consumer retries range-unsupported URLs as sequential streams and probes MPEG-TS. Tests verify chunked response prefix, forwarded header, range rejection, cancellation, MPEG-TS first-PES startup with hint and probe. Full media and consumer CGO=0 audio/player plus CGO=1 audio pass. Sequential WebM/Ogg still require an external rewind-capable spool; no false random-access capability is advertised. |
| 73 | Bound complete initialization and expose Open diagnostics | Done | 2026-09-06 | Codex `/root` | MaxInitBytes counts all Source reads including probing/rereads, returns errors.Is-compatible ErrInitLimit and disables the limit after Open. Synchronous OnOpen reports format, phase, elapsed time, bytes, read/seek calls and original error on success/failure. 1/4/12-byte boundaries and budget-disabled playback are tested; full CGO=0 media suite passes. Source-internal prefetch is explicitly excluded; context deadlines bound blocking I/O. |
| 74 | Apply initialization budget and diagnostics in lavalink-go | Done | 2026-09-06 | Codex `/root` | Consumer Open caps initialization reads at 64MiB and emits debug counters for format/phase/bytes/reads/seeks/elapsed/failure without logging transport error URLs. Consumer non-CGO audio/player suites pass with the local puremux module. |
| 75 | Clear all repository Staticcheck diagnostics including reported SA4003 | Done | 2026-09-06 | Codex `/root` | Removed unused internal helpers and overwritten fixture assignments, asserted the previously ignored corrupt-input result, removed FLAC block-size initializer overwritten in every switch branch, and simplified two server declarations. No bitfield behavior changed; existing specification-derived boundary suites and full CGO=0 tests pass. Staticcheck v0.8.1 reports zero diagnostics; the reported DASH SA4003 was fixed in task 65. |
| 76 | Keep consumer restart and passthrough checks inexpensive and cancellation-safe | Done | 2026-09-06 | Codex `/root` | Producer restart resets the read epoch even when a queued seek has already expired before entering SeekContext. Immutable Opus stream eligibility is computed once; AllocsPerRun verifies zero per-packet allocations. Fully inaudible preroll packets on an unfiltered passthrough stream are skipped without requiring a decoder; partial packet trimming still requires the codec backend. Consumer CGO=0 audio/player suites pass. |
| 77 | Document playback API changes, resolution matrix and verified integration | Done | 2026-09-06 | Codex `/root` | Updated architecture, Unreleased notes and audit resolution for all 12 findings; preserved original audit as historical evidence. Consumer local ignored go.work connects ../puremux, with published go.mod remaining v0.2.2 until a later release. Full consumer CGO race now passes using cached libdave 1.1.0, superseding task 70's missing-pkg-config limitation. Library build/tidy-diff, both full non-CGO suites/vet, library staticcheck/race and diff checks pass; no actual Discord playback or new release claimed. |
| 78 | Advertise manifest-aware live retry intervals | Done | 2026-09-06 | Codex `/root` | LiveWaitError preserves ErrNoNewSegments and exposes RetryAfter; HLS unchanged reload uses half TargetDuration (RFC8216 6.3.4), DASH parses minimumUpdatePeriod and subtracts MPD retrieval elapsed time (DASH-IF validity model). Consumer waits the advertised interval interruptibly, with 250ms fallback. Manifest/media and consumer CGO=0 audio/player tests pass, including ISO duration and sentinel/RetryAfter contracts. Sources linked in architecture. |
| 79 | Convert padding with the decoder output sample rate | Done | 2026-09-06 | Codex `/root` | Consumer FFmpeg bridge derives skip-sample counts from the opened decoder's actual sample rate rather than a potentially different container hint, without mutating the input Packet. Native decoding regression deliberately supplies a 16kHz packet hint and still verifies correct 480-sample output after 10ms trim at 48kHz. Packet.Free clears new trim fields. Full CGO audio suite passes. |
| 80 | Complete cross-repository verification and consumer handoff documentation | Done | 2026-09-06 | Codex `/root` | Final library full race, Staticcheck v0.8.1, vet and diff checks pass. Consumer full non-CGO suite passes; full CGO race passed, then changed audio/player/httpproxy race checks passed again after final manifest/trim fixes. Consumer CGO vet and Windows executable build pass using cached libdave; artifact F:/cache/puremux-lavalink-audit-20260905/lavalink-playback-fixed.exe. Added consumer docs/puremux-playback-fixes.ko.md with local workspace, unpublished dependency requirement, behavior and actual-playback limitations. No new release/push. |
| 81 | Prepare v0.2.3 release | Done | 2026-09-06 | Codex `/root` | User explicitly authorized v0.2.3 publication and consumer upgrade. Promoted Unreleased changelog to 2026-09-06 v0.2.3 and updated integration release plan. Remote main matches v0.2.2 and v0.2.3 does not exist. Fresh uncached CGO=0 full tests, vet, tidy-diff and diff checks pass; preceding full race/Staticcheck verification remains applicable to unchanged code. |
| 82 | Publish puremux v0.2.3 | Done | 2026-09-06 | Codex `/root` | Committed release as 372fdedf164636d3a4c03517c6f80fec78b57e65 and atomically pushed main plus annotated v0.2.3 tag to GitHub. Remote ls-remote verifies main and peeled tag both match the release commit. Existing tag-only release convention retained. |
| 83 | Upgrade lavalink-go to the published v0.2.3 module | Done | 2026-09-06 | Codex `/root` | Consumer go.mod/go.sum now require downloaded puremux v0.2.3 with no replace. Archived temporary go.work/go.work.sum under F:/cache/puremux-lavalink-audit-20260905/v0.2.3-workspace-backup; ordinary go list resolves v0.2.3. With GOWORK=off, full non-CGO tests/vet/tidy-diff and full CGO race/build pass. Consumer deployment documentation updated; executable F:/cache/puremux-lavalink-audit-20260905/lavalink-puremux-v0.2.3.exe. |
| 84 | Publish verified lavalink-go integration and record release completion | Done | 2026-09-06 | Codex `/root` | Consumer commit 0375f93415397ea7d7059ca273d0ae9994be49c7 containing the integration fixes plus v0.2.3 dependency was pushed to its origin/main; remote ls-remote matches HEAD and working tree is clean. Library audit now records published tag commit and actual no-workspace consumer verification. Remaining library changes are publication documentation only. |

## Work log

- 2026-09-06 - Codex `/root` - Task 84: Publish verified lavalink-go integration and record release completion. Consumer commit 0375f93415397ea7d7059ca273d0ae9994be49c7 containing the integration fixes plus v0.2.3 dependency was pushed to its origin/main; remote ls-remote matches HEAD and working tree is clean. Library audit now records published tag commit and actual no-workspace consumer verification. Remaining library changes are publication documentation only.

- 2026-09-06 - Codex `/root` - Task 83: Upgrade lavalink-go to the published v0.2.3 module. Consumer go.mod/go.sum now require downloaded puremux v0.2.3 with no replace. Archived temporary go.work/go.work.sum under F:/cache/puremux-lavalink-audit-20260905/v0.2.3-workspace-backup; ordinary go list resolves v0.2.3. With GOWORK=off, full non-CGO tests/vet/tidy-diff and full CGO race/build pass. Consumer deployment documentation updated; executable F:/cache/puremux-lavalink-audit-20260905/lavalink-puremux-v0.2.3.exe.

- 2026-09-06 - Codex `/root` - Task 82: Publish puremux v0.2.3. Committed release as 372fdedf164636d3a4c03517c6f80fec78b57e65 and atomically pushed main plus annotated v0.2.3 tag to GitHub. Remote ls-remote verifies main and peeled tag both match the release commit. Existing tag-only release convention retained.

- 2026-09-06 - Codex `/root` - Task 81: Prepare v0.2.3 release. User explicitly authorized v0.2.3 publication and consumer upgrade. Promoted Unreleased changelog to 2026-09-06 v0.2.3 and updated integration release plan. Remote main matches v0.2.2 and v0.2.3 does not exist. Fresh uncached CGO=0 full tests, vet, tidy-diff and diff checks pass; preceding full race/Staticcheck verification remains applicable to unchanged code.

- 2026-09-06 - Codex `/root` - Task 80: Complete cross-repository verification and consumer handoff documentation. Final library full race, Staticcheck v0.8.1, vet and diff checks pass. Consumer full non-CGO suite passes; full CGO race passed, then changed audio/player/httpproxy race checks passed again after final manifest/trim fixes. Consumer CGO vet and Windows executable build pass using cached libdave; artifact F:/cache/puremux-lavalink-audit-20260905/lavalink-playback-fixed.exe. Added consumer docs/puremux-playback-fixes.ko.md with local workspace, unpublished dependency requirement, behavior and actual-playback limitations. No new release/push.

- 2026-09-06 - Codex `/root` - Task 79: Convert padding with the decoder output sample rate. Consumer FFmpeg bridge derives skip-sample counts from the opened decoder's actual sample rate rather than a potentially different container hint, without mutating the input Packet. Native decoding regression deliberately supplies a 16kHz packet hint and still verifies correct 480-sample output after 10ms trim at 48kHz. Packet.Free clears new trim fields. Full CGO audio suite passes.

- 2026-09-06 - Codex `/root` - Task 78: Advertise manifest-aware live retry intervals. LiveWaitError preserves ErrNoNewSegments and exposes RetryAfter; HLS unchanged reload uses half TargetDuration (RFC8216 6.3.4), DASH parses minimumUpdatePeriod and subtracts MPD retrieval elapsed time (DASH-IF validity model). Consumer waits the advertised interval interruptibly, with 250ms fallback. Manifest/media and consumer CGO=0 audio/player tests pass, including ISO duration and sentinel/RetryAfter contracts. Sources linked in architecture.

- 2026-09-06 - Codex `/root` - Task 77: Document playback API changes, resolution matrix and verified integration. Updated architecture, Unreleased notes and audit resolution for all 12 findings; preserved original audit as historical evidence. Consumer local ignored go.work connects ../puremux, with published go.mod remaining v0.2.2 until a later release. Full consumer CGO race now passes using cached libdave 1.1.0, superseding task 70's missing-pkg-config limitation. Library build/tidy-diff, both full non-CGO suites/vet, library staticcheck/race and diff checks pass; no actual Discord playback or new release claimed.

- 2026-09-06 - Codex `/root` - Task 76: Keep consumer restart and passthrough checks inexpensive and cancellation-safe. Producer restart resets the read epoch even when a queued seek has already expired before entering SeekContext. Immutable Opus stream eligibility is computed once; AllocsPerRun verifies zero per-packet allocations. Fully inaudible preroll packets on an unfiltered passthrough stream are skipped without requiring a decoder; partial packet trimming still requires the codec backend. Consumer CGO=0 audio/player suites pass.

- 2026-09-06 - Codex `/root` - Task 75: Clear all repository Staticcheck diagnostics including reported SA4003. Removed unused internal helpers and overwritten fixture assignments, asserted the previously ignored corrupt-input result, removed FLAC block-size initializer overwritten in every switch branch, and simplified two server declarations. No bitfield behavior changed; existing specification-derived boundary suites and full CGO=0 tests pass. Staticcheck v0.8.1 reports zero diagnostics; the reported DASH SA4003 was fixed in task 65.

- 2026-09-06 - Codex `/root` - Task 74: Apply initialization budget and diagnostics in lavalink-go. Consumer Open caps initialization reads at 64MiB and emits debug counters for format/phase/bytes/reads/seeks/elapsed/failure without logging transport error URLs. Consumer non-CGO audio/player suites pass with the local puremux module.

- 2026-09-06 - Codex `/root` - Task 73: Bound complete initialization and expose Open diagnostics. MaxInitBytes counts all Source reads including probing/rereads, returns errors.Is-compatible ErrInitLimit and disables the limit after Open. Synchronous OnOpen reports format, phase, elapsed time, bytes, read/seek calls and original error on success/failure. 1/4/12-byte boundaries and budget-disabled playback are tested; full CGO=0 media suite passes. Source-internal prefetch is explicitly excluded; context deadlines bound blocking I/O.

- 2026-09-06 - Codex `/root` - Task 72: Support unknown-length range-free HTTP MPEG-TS input. Added caller-owned-client HTTPStreamSource with terminal read cancellation and explicit sequential probing option; existing no-consumption default without FormatHint remains intact. Consumer retries range-unsupported URLs as sequential streams and probes MPEG-TS. Tests verify chunked response prefix, forwarded header, range rejection, cancellation, MPEG-TS first-PES startup with hint and probe. Full media and consumer CGO=0 audio/player plus CGO=1 audio pass. Sequential WebM/Ogg still require an external rewind-capable spool; no false random-access capability is advertised.

- 2026-09-06 - Codex `/root` - Task 71: Separate consumer audible seek target from decoder preroll. Selected-audio time bases and backward seeks work across puremux, cache and FFmpeg; cached seeks honor direction, retain previous cursor on failure and expose nonzero origin. Preroll becomes bounded per-packet skip metadata, initial Opus pre-skip resets after seek, and packet timestamp rescale overflow is rejected. Consumer CGO=0 audio/player and CGO=1 audio suites pass, including selected 48kHz track request at 420ms for 500ms audible target, native FFmpeg seek tests and combined preroll/end-trim bounds.

- 2026-09-05 - Codex `/root` - Task 70: Retain consumer packet trim and gate Opus passthrough on configuration. Consumer carries DiscardPadding to FFmpeg AV_PKT_DATA_SKIP_SAMPLES (header-derived u32 little-endian start/end plus reasons). Actual libavcodec decoding verifies 960-sample silence becomes 480 samples for either 10ms front or end trim. Passthrough requires stereo 48kHz, mapping 0, zero gain/pre-skip/CodecDelay and no trim; no-CGO EOF regression uses explicitly untrimmed RFC fixtures. Consumer audio/player CGO=0 and audio CGO=1 suites pass. CGO player build is externally blocked by absent dave.pc; no Discord audibility claim.

- 2026-09-05 - Codex `/root` - Task 69: Propagate consumer seek cancellation, live waits and packet timing. lavalink-go seek requests carry the six-second context through demux I/O; fresh playback context survives failed seek; timestamp arithmetic is bounded. Live ErrNoNewSegments waits interruptibly instead of reopening. DurationKnown/Info and metadata refresh dynamically; Opus gaps use previous actual packet duration. Consumer audio/player suites pass using local replacement, including timed seek cancellation, live wait/cancel, late metadata and contiguous 2.5–120ms Opus timing tests.

- 2026-09-05 - Codex `/root` - Task 68: Read fragmented MP4 one movie fragment at a time. Public Open stops after first moof; NextSample discovers subsequent fragments. Failed full-index seeks preserve playback state; partial moof parsing rolls back appended samples. Gated-download regression verifies first packet before tail, failed seek and second-fragment PTS. Full media/MP4 suites pass with CGO disabled, including independent mp4ff structural validation, big-endian ISO BMFF fixture and malformed/truncated boundary tests.

- 2026-09-05 - Codex `/root` - Task 67: Open Ogg from headers and discover duration during playback. Public Open uses header-only initialization without SeekEnd; playback validates page sequence/CRC and discovers EOS duration. Explicit seek builds a temporary index and restores playback on failure. RFC7845 little-endian OpusHead/granules and RFC6716 F8 20ms packets verify gated-prefix startup, failed-index cursor restoration and late 40ms duration; existing malformed/truncated/CRC boundary suites plus full media/Ogg tests pass (CGO_ENABLED=0).

- 2026-09-05 - Codex `/root` - Task 66: Preserve initialization transport and cancellation error chains. Open wraps ErrInvalidData and underlying causes with %w; WebM EBML/Segment reads retain transport errors. Table tests cover cancellation, deadlines and transport failures across probing, WebM, Ogg and MP4. Full media and WebM suites pass with CGO_ENABLED=0.

- 2026-09-05 - Codex `/root` - Task 65: Remove impossible DASH duration bound check (SA4003). time.Duration already has int64 range; removed always-false comparison and unused math import. CGO_ENABLED=0 focused DASH/segment timestamp tests pass.

- 2026-09-05 - Codex `/root` - Task 64: Add HTTP low-latency selection and share Opus timing/config validation. ReadAheadBytes=-1 bypasses prefetch; consumer HTTP source selects it. Exposed opus.PacketSamples/PacketDuration and delegated core/consumer TOC parsers; consumer dOps uses shared HeadFromDOPS. Prefix HTTP and forbidden140ms regressions now pass, RFC6716 MSB-first TOC/count and120ms cap confirmed. Focused library tests and consumer audio/player via local modfile pass.

- 2026-09-05 - Codex `/root` - Task 63: Audit puremux integration and reusable playback features for lavalink-go. Read consumer audio/player/spool paths and instructions. Existing non-CGO audio/httpproxy/player tests pass with released v0.2.2 and temporary modfile pointing at local puremux. Four cache-only overlay tests intentionally fail: HTTP32KiB read-ahead waits past available prefix, fMP4/Ogg eager tail access, consumer Opus140ms accepted. Spec-derived AAC/Opus/Ogg byte values and bit order documented. Added integration audit with 12 ranked findings, explicit static/runtime distinction and generic feature ownership. No product code or consumer module files changed; Discord/CGO end-to-end not tested.

- 2026-09-05 - Codex `/root` - Task 62: Verify and document deferred WebM initialization fix. Fresh full uncached CGO_ENABLED=0 tests, vet/build, complete CGO_ENABLED=1 race suite and diff check pass. Startup regression accesses 76/1,048,672 bytes before first packet with both known/unknown length. Independent libwebm/public ContextSource coverage passes. Architecture and Unreleased changelog document prefix startup, deferred validation/tags, required metadata layout and explicit Seek potentially waiting for the tail. User's original video/Discord output not exercised; changes remain local and unreleased.

- 2026-09-05 - Codex `/root` - Task 61: Preserve lazy WebM metadata visibility and Remux loss checks. Info exposes tags collected during playback. Remux rechecks metadata after draining lazy inputs so trailing tags still require explicit AllowMetadataLoss; RemuxFiles retains failure-before-install semantics. Spec-derived RFC9559 Tags/Tag/SimpleTag bytes and MSB-first lengths verify delayed visibility, default rejection and opt-in success. Focused media/WebM tests pass.

- 2026-09-05 - Codex `/root` - Task 60: Verify public spool startup and deferred WebM seek/metadata boundaries. Public ContextSource Open + first packet succeeds on unchanged libwebm golden with SeekEnd and tail access forbidden. Internal tests cover trailing Cues/Tags collection, immediate track-specific forward/backward cue seek, failed index preserving pending fixed-size lacing, cancellation, nil/truncated headers and truncated block consumption. RFC8794 MSB-first VINTs, RFC9559 BE timestamps/relative offsets and fixed-lacing bytes hand-derived; focused suites pass.

- 2026-09-05 - Codex `/root` - Task 59: Open WebM incrementally without whole-file indexing or SeekEnd. Stop startup at first Cluster after Info/Tracks; collect clusters/Cues/Tags during playback; defer complete unordered/track-specific seek index to explicit Seek with cursor rollback on failure. Optional known byte size avoids SeekEnd; unknown length discovered by reads. Spec-derived MSB-first VINT/BE timestamp fixture and independent libwebm golden both yield first packet with tail unavailable; known/unknown sizes, failed seek resume and deduplicated retry pass. Focused webm/media tests pass.

- 2026-09-05 - Codex `/root` - Task 58: Prepare v0.2.2 release and verify publication candidate. User authorized release through remote. Added v0.2.2 changelog with explicit metadata/discard-padding behavior changes and resource limits; corrected fragment memory contract. Fresh uncached non-CGO tests, vet/build, tidy -diff, module verification and diff checks pass; previous integrated race/fuzz results remain valid (no product edits since). Authenticated HTTPS fetch confirms remote main matches v0.2.1 parent and v0.2.2 is absent. Publication uses main plus annotated v0.2.2 tag, following existing tag-only releases.

- 2026-09-05 - Codex `/root` - Task 57: Publish audit fix matrix and compatibility limits. Added AUDIT_FIXES_2026-09-05.md; original audit clearly marked historical. Documented all 13 runtime fixes, bounds/performance changes, explicit metadata/trim errors, full verification, and unchanged product scope plus linear seek/in-memory TS limits. Documentation diff check passes.

- 2026-09-05 - Codex `/root` - Task 56: Verify integrated audit fixes. CGO_ENABLED=0 go test -count=1 ./..., go vet ./..., go build ./...; CGO_ENABLED=1 go test -race -count=1 ./... all pass. Opus inverse/config fuzz 10s, 1,274,505 executions passes. gofmt and git diff --check clean. Original 15 top-level audit regressions now pass.

- 2026-09-05 - Codex `/root` - Task 55: Align public output contracts and fuzz coverage with fixes. TS rejects unsupported stream metadata unless explicitly allowed and rejects discard padding; audio default semantics retained. Architecture documents unknown TS duration exception, enforcer comments reflect pressure emission. Added inverse Opus conversion to fuzz target. Focused tests pass.

- 2026-09-05 - Codex `/root` - Task 54: Verify aggregate parser limits and shared TS NAL validation. TS pending count/byte limits exercised without OOM; HEVC MSB-first type20 header 28 01 accepted, forbidden 28 00/a8 01 and truncated/nil rejected. Ogg continued packet exact16MiB accepted, oversize/truncated rejected; unsigned lacing and continued flag derived from Ogg layout. Focused suites pass.

- 2026-09-05 - Codex `/root` - Task 53: Eliminate redundant progressive MP4 seek passes. Choose requested/fallback samples in one scan and retain chosen cursor state, avoiding rebuild to chosen index. Exact-tick, B-frame, backward/keyframe, empty-stss regression tests pass. Cursor timestamp overflow now propagates corruption.

- 2026-09-05 - Codex `/root` - Task 52: Preserve supported track metadata and make unsupported loss explicit. MP4 ISO-639-2 language; EBML language/name/default flag roundtrip. MP4/EBML unsupported stream metadata and Remux container tags require explicit AllowMetadataLoss. Discard-padding rejection tested. Spec-derived 5-bit MSB-first kor=0x2df2 verified; backend tests pass.

- 2026-09-05 - Codex `/root` - Task 51: Coalesce sequential HTTP reads with bounded read-ahead. 32 KiB sequential cache; ReaderAt remains independent; explicit Seek revalidates validators. 1,000 single-byte reads require probe + one data request. Source mutation and closed-cache reads rejected; full pkg/media tests pass.

- 2026-09-05 - Codex `/root` - Task 50: Bound fragmented writer metadata and remove repeated payload copying. Incremental fragment duration; 65,536-packet cap; direct mdat header/payload writes; discard pooled buffers larger than 1 MiB. Tiny-packet cap roundtrip, existing independent parser/GOP/overflow tests pass. ASC spec-derived MSB-first; MP4 header size big-endian; packet-count boundary verified.

- 2026-09-05 - Codex `/root` - Task 49: Close remaining manifest and output failure paths. Bound active HLS init cache; checked seek subtraction; recognize terminal manifests without new segments. Fault injection verifies failed output rollback preserves original backup and reports both errors/path. MP4 rejects unsupported discard padding rather than silently dropping it. go test ./pkg/media passes.

- 2026-09-05 - Codex `/root` - Task 48: Repair TS video remux and bound transport ingestion. Unknown video duration stays unknown and TS serialization permits it because PES has no duration field; other muxers retain strict duration checks. Indexed TS reuses incremental parsing, avoiding full-file and completed-PES duplicate copies, with bounded packet/byte retention. Streaming startup rejects EOF before track readiness; detector dispatch shares core validation. Focused TS/PES/stream/remux suites and TS identity audit regression pass with CGO disabled; existing MSB-first PES timestamp and malformed boundary bytes retained.

- 2026-09-05 - Codex `/root` - Task 47: Preserve Ogg EOS packet starts and expose tail trimming. EOS timing follows previous completed granule or the RFC7845 initial-EOS rule; public packets expose DiscardPadding. Removed redundant adapter payload copy and bounded continued-packet retention to 16 MiB. Ogg/media suites and spec-derived little-endian 960/1440 granule regression pass; existing malformed/truncated/CRC and mapping boundaries pass.

- 2026-09-05 - Codex `/root` - Task 46: Guarantee progress when live jitter buffers fill. Capacity overflow now emits the earliest ordered packet instead of discarding it before its time window matures. Enforcer/live suites and 100-packet audit starvation regression pass; ordered packets are retained and the existing zero-window and timestamp-saturation boundaries remain covered.

- 2026-09-05 - Codex `/root` - Task 45: Normalize reverse Opus and FLAC container configurations. Added validated dOps-to-OpusHead conversion and dfLa-to-Matroska normalization. Focused Opus/FLAC/media suites and both audit conversion regressions pass. RFC7845 little-endian Head versus big-endian dOps fields and signed gain are round-trip checked; nil/truncated/invalid-version and existing reserved/mapping boundaries covered.

- 2026-09-05 - Codex `/root` - Task 44: Preserve exact MP4 clocks, sync tables, and lazy payload access. Exact track-tick seek and rational comparisons avoid nanosecond loss; edit shifts apply to DTS/PTS with checked rescaling; empty stss remains distinct from absence. Bounded top-level parsing seeks over mdat and sorts fragments once. Focused progressive/fragmented/media suites and audit seek/origin/delay/overflow/stss/mdat tests pass. Big-endian ISO BMFF fields and truncation/overflow boundaries confirmed; opaque payloads unchanged.

- 2026-09-05 - Codex `/root` - Task 43: Repair manifest recovery, cancellation, and redirect base. Focused HLS/DASH suites and audit recovery/EOF/deadlock/redirect regressions pass with CGO disabled. EOF transitions remain retryable; Close cancels nested reads; bounded fetch uses final retrieval URI. Seek translation uses read shift; exact MP4 seek verified next.

- 2026-09-05 - Codex `/root` - Completed task 42: reviewed edge cases,
  concurrency, demux/mux compatibility, and performance; recorded 13 runtime
  root causes with 15 failing audit tests and additional static findings in
  `docs/AUDIT_2026-09-05.md`. Existing non-CGO, vet, and race gates pass.
  Audit artifacts remain under `F:/cache/puremux-audit-20260905`; product
  implementation and existing tests are unchanged. Fixes remain open.

- 2026-09-05 - Codex `/root` - Completed task 41: added and documented the
  generic `LiveMuxer`, fixed every review finding, passed the full 74.0%-coverage
  release matrix, and completed `$code-review` with no remaining findings.
- 2026-09-05 - Codex `/root` - Completed task 40: corrected the intended
  release designation to v0.2.0 throughout the changelog, architecture,
  implementation ledger, and migration guide; discarded the superseded local
  release commit and tag while preserving the verified implementation.
- 2026-09-05 - Codex `/root` - Completed task 39: aligned the v0.2.0
  changelog date with the publication date, confirmed the branch is based on
  current `origin/main`, and confirmed there is no conflicting local or remote
  v0.2.0 tag before preparing the release commit.
- 2026-09-05 - Codex `/root` - Completed task 38: re-ran the complete final
  v0.2.0 verification matrix after the AV1 reserved-profile fix. All code,
  test, race, fuzz, vulnerability, cross-build, interoperability, module,
  format, dependency, and whitespace gates pass; the sole release blocker is
  committing the current `v0.1.1-dirty` tree before creating the v0.2.0 tag.
- 2026-09-05 - Codex `/root` - Completed task 37: extended the AV1
  configuration profile guard from reserved value 3 to the complete reserved
  3-7 range, added the missing MSB-first profile-4 boundary, and re-passed
  focused plus full non-CGO tests, vet, and formatting.
- 2026-09-05 - Codex `/root` - Completed task 36: synchronized release and
  architecture documentation and re-ran the full post-fix release matrix.
  Non-CGO coverage is 73.5%; repeated, 32-bit, race, six cross-build,
  interoperability, vulnerability, dependency, format, and whitespace gates
  pass, and 13 fuzz targets completed 3,574,487 executions without panic. All
  task-31 code/specification blockers are closed; commit remains required
  before tagging the dirty working tree.
- 2026-09-05 - Codex `/root` - Completed task 35: made CodecDelay presence
  codec-aware so zero-pre-skip Opus still writes the mandatory zero-valued
  element; exact EBML bytes and the public WebM mux path pass focused non-CGO
  tests.
- 2026-09-05 - Codex `/root` - Completed task 34: converted complete valid
  Matroska FLAC metadata chains to canonical MP4 dfLa, validated retained MP4
  dfLa records, rejected non-final minimal chains, and passed RFC 9639-derived
  boundary tests across FLAC, MP4, and public media with CGO disabled.
- 2026-09-05 - Codex `/root` - Completed task 33: split VP9's Matroska
  feature-metadata and MP4 vpcC representations, added checked conversion in
  both remux directions, enforced all registered semantic fields and zero
  initialization-data size, and passed spec-derived boundary, fuzz-seed, MP4,
  WebM, and public media tests with CGO disabled.
- 2026-09-05 - Codex `/root` - Completed task 32: closed the AV1 MP4
  signalling and configuration gaps with the mandatory `av01` brand,
  conditional nclx colour information, and bounded configOBU validation;
  spec-derived MSB-first header bytes and all malformed boundaries pass the
  focused non-CGO AV1, MP4, and media suites.
- 2026-09-05 - Codex `/root` - Completed task 31: all executable release
  gates pass on the current working tree, including 73.1% coverage, race,
  six-target non-CGO cross-builds, govulncheck, and 1,593,009 fuzz mutation
  executions without panic. Official AV1-ISOBMFF, VP9-in-Matroska/MP4,
  RFC 9639 FLAC, and Matroska Opus rules exposed four release-blocking
  conformance/interoperability gaps. The tree remains `v0.1.1-dirty`;
  v0.2.0 is a no-go pending fixes, spec-derived boundary tests, a full gate
  rerun, and a commit before tagging.
- 2026-09-05 - Codex `/root` - Completed task 30: re-ran the complete final
  release matrix after every audit fix; non-CGO, shuffled x10, Windows/386,
  race, vet/module/format/dependency/whitespace, real-fixture interop, and six
  cross-build gates pass with 73.1% statement coverage and no remaining task
  21 blocker.
- 2026-09-05 - Codex `/root` - Completed task 29: eliminated an AVC High
  Profile false-pass record, enforced its conditional avcC extension fields,
  re-derived Baseline and High Profile bytes, passed focused parser/container/
  mp4ff tests, and completed 1,490,115 fuzz executions without panic.
- 2026-09-05 - Codex `/root` - Completed task 28: added Matroska FLAC
  metadata-chain validation to the seeded FLAC fuzz surface and completed
  1,469,423 mutation executions without panic.
- 2026-09-05 - Codex `/root` - Completed task 27: preserved complete valid
  Matroska FLAC metadata chains and rejected malformed block ordering, sizes,
  and finalization using spec-derived MSB-first metadata headers; focused FLAC
  and public media tests pass with CGO disabled.
- 2026-09-05 - Codex `/root` - Completed task 26: added AV1 and Vorbis
  configuration fuzz targets and completed 1,358,911 seeded mutation
  executions without panic, alongside passing deterministic tests.
- 2026-09-05 - Codex `/root` - Completed task 25: isolated the new codec
  configuration inspection in bitstream packages, shared AVC/HEVC/AV1
  validators across MP4 and EBML, and passed isolated boundary suites plus
  focused container/public API tests with CGO disabled.
- 2026-09-05 - Codex `/root` - Completed task 24: enforced and normalized
  mandatory EBML codec initialization for AVC, HEVC, AV1, FLAC, and Vorbis;
  verified spec-derived bit packing, public Vorbis round-trip preservation,
  and nil/truncated/malformed/overrun/property-mismatch boundaries in the
  focused non-CGO media suite.
- 2026-09-05 - Codex `/root` - Completed task 23: made Matroska cluster
  timestamp plus signed Block-relative timecode overflow-safe and verified
  both rejection and the adjacent valid int64 boundary with spec-derived
  big-endian Block bytes in the focused non-CGO WebM suite.
- 2026-09-05 - Codex `/root` - Completed task 22: corrected HEVC temporal-ID
  validation for keyframe and configuration-only detection, then verified the
  spec-derived MSB-first valid, forbidden, nonzero-IRAP, and truncated header
  boundaries with the focused non-CGO core suite.
- 2026-09-05 - Codex `/root` - Completed task 21: re-ran all executable
  release, race, 32-bit, cross-build, coverage, fuzz, interoperability,
  dependency-vulnerability, formatting, and module gates. All executable
  gates pass, but an official IETF/ITU specification audit found three
  release-blocking validation/arithmetic gaps in EBML codec initialization,
  HEVC NAL-header validation, and extreme Matroska timestamp addition.
- 2026-09-05 - Codex `/root` - Completed task 20: invoked Go from MSYS2
  UCRT64 with its GCC 16.1.0 toolchain and confirmed the complete race-enabled
  repository test suite passes, resolving the prior PowerShell/cgo failure.
- 2026-09-04 - Codex `/root` - Completed task 19: all repository-owned
  release gates, repeated/randomized tests, Windows/386 execution, six
  cross-platform builds, and parser fuzz runs pass after the audit fixes;
  coverage is 72.8%. Isolated the remaining Windows race failure to Go
  1.27.0's standard `runtime/cgo` build before any puremux package compiles.
- 2026-09-04 - Codex `/root` - Completed task 18: normalized the last
  non-idiomatic WebM import groups and rechecked formatting and focused tests.
- 2026-09-04 - Codex `/root` - Completed task 17: removed the last stale
  production-source reference to the non-standard Matroska HEVC CodecID while
  retaining the audit history that explains the correction.
- 2026-09-04 - Codex `/root` - Completed task 16: covered all seven MP4
  output codec sample entries, added ten parser/framing fuzz targets, executed
  every target under mutation without a crash, and normalized all Go sources
  until `gofmt -l .` returned no files.
- 2026-09-04 - Codex `/root` - Completed task 15: removed every audited
  uint32-to-int overflow path before slicing or offset addition and confirmed
  the full repository test suite passes natively as Windows/386 with CGO
  disabled, including maximal malformed comment and NAL lengths.
- 2026-09-04 - Codex `/root` - Completed task 14: tightened FLAC, Opus,
  AVC, and HEVC parsers and MP4 admission checks against their published bit
  layouts, including forbidden limits, reserved bits, parameter-set NAL types,
  truncated records, and both Opus byte orders.
- 2026-09-04 - Codex `/root` - Completed task 13: aligned Matroska HEVC,
  Opus initialization/timing metadata, and unknown dependency references with
  the registered mappings, then verified exact EBML bytes and public
  writer-to-reader behavior with specification-derived little-endian OpusHead
  values and malformed-boundary cases.
- 2026-09-04 - Codex `/root` - Completed task 12: independently re-ran all
  release gates plus repeated shuffled tests, coverage, module integrity, and
  cross-platform builds; the native amd64 gates pass, but the audit found
  specification and 32-bit boundary defects that prevent a clean release
  verdict.
- 2026-09-04 - Codex `/root` - Compared codec/container validation with RFC
  9639, RFC 7845, ISO AVC configuration syntax, and the current Matroska
  element/codec mappings. Confirmed invalid FLAC/Opus/AVC/HEVC records can be
  accepted, Matroska HEVC uses the wrong CodecID, and Opus output omits
  required initialization timing elements.
- 2026-09-04 - Codex `/root` - A Windows/386 run exposed a compile-time NAL
  size comparison overflow and a malformed OpusTags integer-overflow panic;
  static review found the same unsafe 32-bit length pattern in Vorbis comments
  and four-byte AVCC NAL walking.

- 2026-09-04 — Codex `/root` — Completed task 11: all release gates pass on
  the final v0.2.0 implementation (`CGO_ENABLED=0` tests, vet, tidy diff, and
  whitespace validation).
- 2026-09-04 — Codex `/root` — During task 11 final audit, fixed signed
  timestamp arithmetic overflow in both MP4 writers and verified extreme
  MinInt64/MaxInt64 inputs are rejected before any output is written.
- 2026-09-04 — Codex `/root` — Corrected progressive `mdhd` duration to the
  decode timeline and added an independent mp4ff assertion for the case where
  a positive composition offset extends presentation beyond decode duration.
- 2026-09-04 — Codex `/root` — Completed task 10: rewrote architecture,
  added the breaking-API migration guide and v0.2.0 changelog, and aligned the
  documented positive-duration invariant with a verified TS boundary check.
- 2026-09-04 — Codex `/root` — Completed task 9: checked progressive and
  fragmented output with mp4ff v0.56.0 using its attributed real H.264
  fixture, including independent sample-offset and payload extraction.
- 2026-09-04 — Codex `/root` — Completed task 8: removed `pkg/puremux`,
  migrated the CLI and remaining tests to `pkg/media`, and verified the new
  `puremux remux -o output.mp4 ...` path plus early cancellation.
- 2026-09-04 — Codex `/root` — Completed task 7: added exact multi-source
  remuxing and atomic file output, then fixed EBML packet-duration preservation
  with specification-derived BlockGroup bytes and end-to-end MP4 round trips.
- 2026-09-04 — Codex `/root` — Completed task 6: exposed direct WebM,
  Matroska, progressive/fMP4, and MPEG-TS writers through `media.NewMuxer` and
  verified public writer-to-reader round trips with `CGO_ENABLED=0`.

- 2026-09-04 — Codex `/root` — Initialized the v0.2.0 plan after reviewing the
  puremux codebase and the MP4/EBML writer implementations, examples, and tests
  in mp4ff, mediacommon, gomedia, and ebml-go.
- 2026-09-04 — Codex `/root` — Completed task 1: exact public muxer contract and
  MP4 mode/limit validation, verified with `CGO_ENABLED=0 go test ./pkg/media`.
- 2026-09-04 — Codex `/root` — Completed task 2: common ISO BMFF box and sample
  entry serializers, verified with spec-derived bytes and boundary cases.
- 2026-09-04 — Codex `/root` — Completed task 3: progressive MP4 serialization
  and exact-timing round-trip verification, including signed ctts v1 bytes.
- 2026-09-04 — Codex `/root` — Completed task 4: bounded non-seekable fMP4
  serialization with pooled buffers and two-fragment exact-timing round trips.
- 2026-09-04 — Codex `/root` — Completed task 5: bounded native codec config
  validation plus OpusHead→dOps and STREAMINFO→dfLa conversions.
