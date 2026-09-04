# Migrating to puremux v0.2.0

v0.2.0 is intentionally breaking. The `pkg/puremux` facade, `Session`,
`Merge`, `Probe`, duration-based `Packet`, and compatibility aliases were
removed. Import `github.com/famomatic/puremux/pkg/media` for all new code.

## File remuxing

Replace legacy `puremux.Merge` with `media.RemuxFiles`:

```go
err := media.RemuxFiles(
    ctx,
    []string{"video.webm", "audio.webm"},
    "output.mp4",
    media.MuxOptions{}, // output format inferred from .mp4
)
```

Supported output extensions are `.mp4`, `.m4a`, `.m4v`, `.webm`, `.mkv`,
`.mka`, `.ts`, and `.m2ts`. MP4 uses progressive layout for the seekable file
destination. File installation is atomic and an existing output is retained
when remuxing fails.

For caller-owned sources and destinations, use `media.Remux`:

```go
err := media.Remux(ctx, []media.RemuxInput{
    {Source: firstSource},
    {Source: secondSource, Options: media.OpenOptions{FormatHint: media.FormatMPEGTS}},
}, destination, media.MuxOptions{Format: media.FormatMP4})
```

`Remux` owns and closes all input sources. It never closes `destination`.

## Direct muxing

Legacy timestamps used `time.Duration`. The new API uses integer ticks in the
stream's exact time base:

```go
muxer, err := media.NewMuxer(output, media.MuxOptions{
    Format:  media.FormatMP4,
    MP4Mode: media.MP4ModeAuto,
})
if err != nil {
    return err
}

streamIndex, err := muxer.AddStream(media.Stream{
    Type:       media.MediaVideo,
    Codec:      media.CodecH264,
    TimeBase:   media.Rational{Num: 1, Den: 90_000},
    Width:      1920,
    Height:     1080,
    Config:     media.CodecConfig{Format: media.CodecConfigAVCC, Data: avcC},
})
if err != nil {
    return err
}

err = muxer.WritePacket(ctx, &media.Packet{
    StreamIndex: streamIndex,
    Data:        lengthPrefixedAccessUnit,
    DTS:         media.KnownTimestamp(0),
    PTS:         media.KnownTimestamp(3_000),
    Duration:    media.KnownTimestamp(3_000),
    Flags:       media.PacketKeyframe,
})
if err != nil {
    return err
}
return muxer.Close()
```

Register every stream before the first packet. PTS, DTS, and positive duration
are required. `WritePacket` is synchronous: after it returns, caller-owned
packet data may be reused. `Close` is idempotent and does not close the output.

`MP4ModeAuto` writes progressive MP4 to an `io.WriteSeeker` and fragmented MP4
to any other `io.Writer`. Set `FragmentDuration` and `MaxFragmentBytes` to
bound fMP4 buffering; zero selects defaults.

## Live MPEG-TS ingestion (v0.2.1)

v0.2.1 provides an explicit replacement for the removed live `Session`
helpers without weakening the ordinary muxer's exact-timestamp contract:

```go
muxer, err := media.NewMuxer(output, media.MuxOptions{Format: media.FormatMPEGTS})
if err != nil {
    return err
}
opts := media.DefaultLiveIngestOptions()
opts.MinMonotonicStep = time.Millisecond
live, err := media.NewLiveMuxer(muxer, opts)
if err != nil {
    return err
}

video, err := live.AddStream(media.Stream{
    Type: media.MediaVideo, Codec: media.CodecH264,
    TimeBase: media.Rational{Num: 1, Den: 1000},
    DefaultPacket: time.Second / 60,
})
if err != nil {
    return err
}
audio, err := live.AddStream(media.Stream{
    Type: media.MediaAudio, Codec: media.CodecAAC,
    TimeBase: media.Rational{Num: 1, Den: 1000},
    SampleRate: 48000, Channels: 2,
})
if err != nil {
    return err
}

if err := live.WriteVideo(ctx, video, annexBAccessUnit, decodeMilliseconds); err != nil {
    return err
}
if err := live.WriteADTS(ctx, audio, oneOrMoreADTSFrames, firstFrameMilliseconds); err != nil {
    return err
}
return live.Close()
```

`WriteVideo` derives keyframe and H.264/HEVC presentation order from bounded
NAL headers. `WriteADTS` splits concatenated frames and advances their times
from the header sample count. Both copy payloads before returning. Register all
tracks before the first write, feed from one goroutine, and always call
`Close` to drain the startup probe and jitter windows. The facade accepts
Annex-B/ADTS framing and is intended for a compatible sink such as MPEG-TS;
it does not convert framing for MP4. One stream time-base tick must fit in a
positive `time.Duration`; duplicate repair is automatically raised to that
granularity, while `MinMonotonicStep` should be set higher when the destination
clock is coarser.

For callers that already have ordinary compressed packets with exact PTS/DTS,
use the same object through its generic `Muxer` method:

```go
err = live.WritePacket(ctx, &media.Packet{
    StreamIndex: video,
    Data: packetData,
    PTS: media.KnownTimestamp(pts),
    DTS: media.KnownTimestamp(dts),
    Duration: media.KnownTimestamp(duration), // optional on LiveMuxer
    Flags: flags,
})
```

This generic path works with every codec/container combination accepted by the
wrapped muxer and preserves packet flags, position, and discard padding. Do not
mix generic `WritePacket`, `WriteVideo`, and `WriteADTS` calls on one stream.

## Probing

Replace legacy `Probe` with `OpenFile` plus `Open`:

```go
source, err := media.OpenFile(path)
if err != nil {
    return err
}
demuxer, err := media.Open(ctx, source, media.OpenOptions{})
if err != nil {
    _ = source.Close() // Open owns it only on success
    return err
}
defer demuxer.Close()

info := demuxer.Info()
streams := demuxer.Streams()
```

## Framing requirements

Puremux does not transcode and does not silently change elementary-stream
framing:

- MP4 H.264/HEVC packets must be length-prefixed and accompanied by avcC/hvcC.
- MPEG-TS H.264/HEVC packets must be Annex-B access units.
- MPEG-TS AAC packets must be complete ADTS frames.
- MP4 AAC packets are raw access units accompanied by ASC.

Use the bounded helpers in `pkg/bitstream` when an application explicitly
chooses to convert framing. Generic `Remux` rejects incompatible MPEG-TS
framing rather than producing a misleading file.

## CLI

The old `merge` command was replaced by `remux` and now supports MP4:

```text
puremux probe input.webm
puremux remux -o output.mp4 input.webm
```
