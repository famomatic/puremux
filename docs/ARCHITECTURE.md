# Puremux v0.2 Architecture

## Purpose

Puremux is a pure-Go compressed-media demuxing and muxing toolkit. It copies
compressed packets between supported containers without decoding audio or
video. v0.2 makes `pkg/media` the only public API and adds native progressive
and fragmented MP4 output.

The module must build with `CGO_ENABLED=0`. Production code may not invoke
FFmpeg/ffprobe, decode pixels or PCM, or depend on a native codec library.

## Public packages

### `pkg/media`

`pkg/media` owns all public container concepts:

- `Source`, `Open`, `Demuxer`, `Stream`, and `Packet` for input;
- `MuxOptions`, `NewMuxer`, and `Muxer` for output;
- `LiveIngestOptions` and `LiveMuxer` for generic, explicitly normalized live
  packet ingestion ahead of any muxer;
- `RemuxInput`, `Remux`, and `RemuxFiles` for exact packet copying;
- HLS/DASH readers and caller-controlled HTTP sources.

Packet PTS, DTS, and duration are signed integer ticks in the owning stream's
`Rational` time base. `time.Duration` is only a boundary convenience. A
demuxer reports container timing without repair, and a muxer serializes the
ordered timing supplied by its caller without repair.

`Open` owns a source only after it succeeds. `Remux` explicitly takes
ownership of every supplied input source. Neither `NewMuxer` nor `Remux`
closes its output writer.

### `pkg/bitstream`

Bitstream packages parse or convert compressed framing and configuration
records without decoding samples. They cover AAC ASC/ADTS, H.264 avcC and
Annex-B/length prefixes, HEVC hvcC and NAL framing, OpusHead/dOps, FLAC
STREAMINFO/dfLa, MP3 headers, and generic NAL conversion.

All codec-specific inspection belongs here or in `internal/core`. Container
writers consume validated, codec-neutral configuration and opaque samples.

## Internal layers

```text
cmd/puremux                 CLI using pkg/media only
pkg/media                   public demux, mux, remux, source, manifest API
pkg/bitstream/<codec>       bounded compressed-header/config transforms
internal/format/mp4         ISO BMFF reader, progressive writer, fMP4 writer
internal/format/webm        Matroska/WebM reader and EBML writer
internal/format/ebml        RFC 8794 integer/VINT primitives
internal/format/mpegts      MPEG-2 TS reader/writer
internal/format/ogg         Ogg reader
internal/manifest           HLS/DASH parsing
internal/core               shared codec and packet internals
internal/preprocessor       isolated timestamp-repair algorithms
```

The preprocessor never writes container bytes. It is not implicitly invoked
by `media.Muxer` or `media.Remux`; callers of the public mux API must provide
pristine, decode-ordered streams. The muxers never reorder, interpolate, or
otherwise repair timestamps.

`media.LiveMuxer` is the sole public opt-in coordinator for the internal
preprocessors. It stays above `Muxer`, owns and bounds every buffered packet,
and emits normalized exact-tick packets through the ordinary mux interface.
This preserves the separation that preprocessors never serialize bytes and
muxers never repair timing.

## Muxing contract

Streams are registered before the first packet. `WritePacket` is synchronous
and not safe for concurrent calls on one muxer. It returns only after the
implementation has copied any bytes it needs to retain, so callers may then
release or reuse the packet and payload. `Close` is idempotent and finalizes
container metadata but does not close the destination.

All muxers require known PTS and DTS. MP4 and EBML require positive known duration; MPEG-TS also accepts unknown duration because PES does not serialize it. Known zero/negative duration remains invalid. Container clock
conversion is explicit:

- MP4 preserves ticks directly and therefore requires an integral
  ticks-per-second time base;
- WebM/Matroska quantizes timestamps and duration to its 1 ms TimecodeScale;
- MPEG-TS converts PTS/DTS to its 90 kHz clock and retains its established
  first-DTS plus 10-second headroom mapping.

No muxer converts elementary-stream framing. H.264/HEVC MP4 samples are
length-prefixed and require avcC/hvcC. MPEG-TS H.264/HEVC packets are Annex-B,
and AAC packets are complete ADTS frames. `Remux` rejects source/output pairs
whose demuxed packet framing cannot satisfy MPEG-TS output.

## Opt-in live ingestion

`LiveMuxer` implements the ordinary `Muxer` interface. `WritePacket` accepts
any compressed packet admitted by the wrapped muxer and preserves its flags,
position, discard padding, payload, and supplied duration. It copies input
bytes, uses a per-stream bounded jitter enforcer, holds video until its first
keyframe, aligns audio to the first registered video stream at packet
granularity, completes missing durations with one-packet lookahead, and merges
streams by DTS before forwarding them.

Two optional ingestion helpers handle sources that do not yet satisfy the
generic packet contract. `WriteVideo` accepts one decode-clock Annex-B
H.264/HEVC access unit and derives B-frame presentation time from picture-order
headers. `WriteADTS` accepts one or more AAC ADTS frames; header sample count
and sample rate determine exact split-frame duration and timestamps. A stream
uses exactly one of generic packets, decode-clock video, or ADTS chunks so no
stateful parser observes a partial sequence.

The facade performs only bounded compressed-header inspection; it never
decodes slice payloads or AAC samples. It derives positive packet durations
with one-packet lookahead, performs a bounded cross-stream DTS merge, exposes
drop/gap metrics, and flushes all stages before closing the wrapped muxer.
Callers should set `MinMonotonicStep` to a value that survives the destination
clock's quantization; 1 ms is a conservative live-output value. The facade
automatically raises the step to at least one stream-time-base tick and rejects
time bases whose individual tick is zero or overflows in its nanosecond
preprocessing domain.

The generic path does not change elementary-stream framing and therefore works
with any compatible muxer. The convenience helpers retain their input framing:
Annex-B video and ADTS AAC are directly compatible with MPEG-TS, not with the
length-prefixed video or raw AAC framing required by MP4.

## MP4 output

### Progressive mode

A seekable destination receives `ftyp`, a 64-bit-size `mdat`, and a final
`moov`. Sample payloads are written immediately; bounded per-sample metadata
is retained until close. Finalization patches `mdat` and writes version-1
movie/track/media headers, edit lists, sample descriptions, run-length
`stts`, signed `ctts` version 1, `stss`, `stsc`, `stsz`, and `stco` or `co64`.

Moov-at-end is deliberate for v0.2. Fast-start relocation is outside this
release.

### Fragmented mode

Any `io.Writer` can receive fragmented MP4. The writer emits an initialization
`ftyp+moov+mvex/trex`, followed by bounded `moof+mdat` fragments. Each fragment
contains `mfhd`, per-track `tfhd`, 64-bit `tfdt`, and signed version-1 `trun`
entries. Payload buffers come from `sync.Pool` and are copied before
`WritePacket` returns.

Video fragments cut on GOP/keyframe boundaries; audio-only output uses the
configured duration threshold. `MaxFragmentBytes` bounds retained payload,
while an internal packet cap bounds fragment metadata. Either cap can force a
non-keyframe cut; they are not a process-wide memory bound.
`MP4ModeAuto` selects progressive output for an `io.WriteSeeker` and fragmented
output otherwise.

### MP4 codec matrix

| Codec | Sample entry | Required configuration |
|---|---|---|
| H.264 | `avc1` | `avcC` |
| HEVC | `hvc1` | `hvcC` |
| AV1 | `av01` | `av1C` |
| VP9 | `vp09` | `vpcC` |
| AAC | `mp4a` | ASC, serialized in `esds` |
| Opus | `Opus` | `dOps` or convertible OpusHead |
| FLAC | `fLaC` | `dfLa`, STREAMINFO, or Matroska FLAC private data |

The configuration validators reject missing parameter sets, malformed sizes,
reserved bits, unsupported channel mappings, and metadata/config mismatches.
AV1 output includes the `av01` compatible brand; an AV1 sample entry whose
`av1C` has no Sequence Header OBU carries an unspecified limited-range
`colr/nclx` box as required by the binding. VP9 `vpcC` validation includes the
registered profile/level/bit-depth/chroma combinations and requires its codec
initialization data size to be zero.

## WebM and Matroska output

Both formats share one EBML implementation and differ by DocType and codec
policy. WebM accepts VP8, VP9, AV1, Opus, and Vorbis. Matroska additionally
accepts H.264, HEVC, and FLAC.

Packets use `BlockGroup` with explicit `BlockDuration`; non-keyframes include
`ReferenceBlock=0` when their exact dependency is unknown, and signed
`DiscardPadding` is retained. VP9 CodecPrivate uses the Matroska ID/length/value
feature list and is converted to or from MP4 `vpcC`; the two representations
are never copied as though they were identical. Complete FLAC CodecPrivate
metadata chains are preserved in Matroska and normalized to STREAMINFO-only
`dfLa` for MP4. Opus tracks carry validated OpusHead, spec-derived CodecDelay
(including a present zero-valued element), and SeekPreRoll. This avoids losing
duration for codecs that do not expose a duration in their packet header.

Seekable outputs patch Segment size and Duration and append Cues plus SeekHead.
Non-seekable outputs use RFC 8794 unknown-size Segment/Cluster VINTs and omit
indexes.

## MPEG-TS output

The TS backend emits a single program carrying Annex-B H.264/HEVC and ADTS
AAC. PAT/PMT are repeated periodically, PCR uses the first video PID (or first
audio PID), and every 188-byte packet is written immediately. There is no
trailer. TS output is useful for explicitly framed live packets; it is not a
general MP4/WebM-to-TS framing converter.

## Exact remuxing

`Remux` opens every input, registers tracks in input order, primes one packet
per source, and performs a stable multiway merge by DTS. Cross-time-base
comparison uses unsigned 192-bit products, so it neither overflows nor loses
sub-nanosecond ordering by converting through `time.Duration`.

Every demuxed packet is released after the synchronous mux write, including
all error paths. `RemuxFiles` writes to a sibling temporary file, rejects an
output that aliases an input, and installs the result only after successful
container finalization and file close.

## Verification gates

Container and codec tests use hand-derived specification bytes with explicit
bit order and malformed/truncated/overflow boundaries. MP4 writer tests also
use an attributed real H.264 access unit and the independent Eyevinn/mp4ff
parser to validate box structure, sample tables, data offsets, timestamps,
and byte-identical payload extraction.

A release is complete only after:

```text
CGO_ENABLED=0 go test -count=1 ./...
go vet ./...
go mod tidy -diff
git diff --check
```
