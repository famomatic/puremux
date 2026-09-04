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
