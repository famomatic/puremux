package media

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/famomatic/puremux/internal/format/webm"
	"github.com/famomatic/puremux/pkg/bitstream/av1"
	"github.com/famomatic/puremux/pkg/bitstream/flac"
	"github.com/famomatic/puremux/pkg/bitstream/h264"
	"github.com/famomatic/puremux/pkg/bitstream/hevc"
	"github.com/famomatic/puremux/pkg/bitstream/opus"
	"github.com/famomatic/puremux/pkg/bitstream/vorbis"
	"github.com/famomatic/puremux/pkg/bitstream/vp9"
)

const ebmlTimecodeScale = uint64(1_000_000)

type positionWriter struct {
	w        io.Writer
	seeker   io.Seeker
	seekable bool
	offset   int64
}

func newPositionWriter(w io.Writer) *positionWriter {
	p := &positionWriter{w: w}
	if seeker, ok := w.(io.Seeker); ok {
		p.seeker = seeker
		p.seekable = true
	}
	return p
}

func (w *positionWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.offset += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *positionWriter) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		return w.offset, nil
	}
	if !w.seekable {
		return 0, ErrNotSeekable
	}
	n, err := w.seeker.Seek(offset, whence)
	if err == nil {
		w.offset = n
	}
	return n, err
}

type ebmlMuxer struct {
	w           *positionWriter
	format      Format
	streams     []Stream
	tracks      []webm.TrackSpec
	header      webm.Header
	cluster     *webm.ClusterWriter
	clusterTime int64
	maxEndTime  int64
	cues        []webm.CuePoint
	started     bool
	closed      bool
	closeErr    error
}

func newEBMLMuxer(w io.Writer, format Format) (Muxer, error) {
	return &ebmlMuxer{w: newPositionWriter(w), format: format}, nil
}

func (m *ebmlMuxer) AddStream(stream Stream) (int, error) {
	if m.closed || m.started {
		return 0, errorsForMuxState(m.closed)
	}
	if !stream.TimeBase.Valid() || stream.TimeBase.Num <= 0 {
		return 0, fmt.Errorf("%w: invalid time base", ErrInvalidData)
	}
	codec := coreCodec(stream.Codec)
	if codec == 0 || !m.codecAllowed(stream.Codec) {
		return 0, fmt.Errorf("%w: %s in %s", ErrUnsupportedCodec, stream.Codec, m.format)
	}
	wantType := MediaAudio
	if codec.IsVideo() {
		wantType = MediaVideo
	}
	if stream.Type != wantType {
		return 0, fmt.Errorf("%w: stream type does not match codec", ErrInvalidData)
	}
	if wantType == MediaVideo && (stream.Width <= 0 || stream.Height <= 0) {
		return 0, fmt.Errorf("%w: invalid video dimensions", ErrInvalidData)
	}
	if wantType == MediaAudio && (stream.Channels <= 0 || stream.SampleRate <= 0) {
		return 0, fmt.Errorf("%w: invalid audio properties", ErrInvalidData)
	}
	if len(m.tracks) >= 16_383 {
		return 0, fmt.Errorf("%w: too many EBML tracks", ErrInvalidData)
	}
	if stream.CodecDelay < 0 || stream.SeekPreRoll < 0 {
		return 0, fmt.Errorf("%w: negative codec timing", ErrInvalidData)
	}
	sampleRate := float64(stream.SampleRate)
	codecDelayNS := uint64(stream.CodecDelay)
	seekPreRollNS := uint64(stream.SeekPreRoll)
	if stream.Codec == CodecOpus {
		if stream.Config.Format != CodecConfigOpusHead {
			return 0, fmt.Errorf("%w: Opus requires OpusHead", ErrUnsupportedCodec)
		}
		config, err := opus.ParseHead(stream.Config.Data)
		if err != nil || config.Channels != stream.Channels {
			return 0, fmt.Errorf("%w: invalid OpusHead", ErrInvalidData)
		}
		expectedDelay := time.Duration(uint64(config.PreSkip) * uint64(time.Second) / 48_000)
		if stream.CodecDelay != 0 && stream.CodecDelay != expectedDelay {
			return 0, fmt.Errorf("%w: Opus codec delay does not match pre-skip", ErrInvalidData)
		}
		codecDelayNS = uint64(expectedDelay)
		if seekPreRollNS == 0 {
			seekPreRollNS = uint64(80 * time.Millisecond)
		}
		if config.InputSampleRate != 0 {
			sampleRate = float64(config.InputSampleRate)
		}
	}
	codecPrivate, err := normalizeEBMLCodecPrivate(stream)
	if err != nil {
		return 0, err
	}
	index := len(m.streams)
	trackNumber := index + 1
	m.tracks = append(m.tracks, webm.TrackSpec{
		Number:        uint64(trackNumber),
		UID:           uint64(trackNumber),
		Codec:         codec,
		IsVideo:       wantType == MediaVideo,
		Width:         stream.Width,
		Height:        stream.Height,
		Channels:      stream.Channels,
		SampleRate:    sampleRate,
		CodecDelayNS:  codecDelayNS,
		SeekPreRollNS: seekPreRollNS,
		CodecPrivate:  codecPrivate,
	})
	copyStream := cloneStreams([]Stream{stream})[0]
	copyStream.Index = index
	m.streams = append(m.streams, copyStream)
	return index, nil
}

func normalizeEBMLCodecPrivate(stream Stream) ([]byte, error) {
	data := stream.Config.Data
	requireFormat := func(want CodecConfigFormat, name string) error {
		if stream.Config.Format != want {
			return fmt.Errorf("%w: %s requires %s codec initialization", ErrUnsupportedCodec, stream.Codec, name)
		}
		return nil
	}
	invalid := func(name string) error {
		return fmt.Errorf("%w: invalid %s codec initialization", ErrInvalidData, name)
	}

	switch stream.Codec {
	case CodecH264:
		if err := requireFormat(CodecConfigAVCC, "AVCDecoderConfigurationRecord"); err != nil {
			return nil, err
		}
		if err := h264.ValidateAVCC(data); err != nil {
			return nil, invalid("AVC")
		}
	case CodecHEVC:
		if err := requireFormat(CodecConfigHVCC, "HEVCDecoderConfigurationRecord"); err != nil {
			return nil, err
		}
		if err := hevc.ValidateHVCC(data); err != nil {
			return nil, invalid("HEVC")
		}
	case CodecAV1:
		if err := requireFormat(CodecConfigAV1C, "AV1CodecConfigurationRecord"); err != nil {
			return nil, err
		}
		if err := av1.ValidateConfig(data); err != nil {
			return nil, invalid("AV1")
		}
	case CodecFLAC:
		if err := requireFormat(CodecConfigFLACStreamInfo, "FLAC STREAMINFO"); err != nil {
			return nil, err
		}
		private, info, err := flac.MatroskaCodecPrivate(data)
		if err != nil || info.SampleRate != stream.SampleRate || info.Channels != stream.Channels {
			return nil, invalid("FLAC")
		}
		return private, nil
	case CodecVorbis:
		if err := requireFormat(CodecConfigVorbisHeaders, "Xiph-laced Vorbis headers"); err != nil {
			return nil, err
		}
		if err := vorbis.ValidateCodecPrivate(data, stream.Channels, stream.SampleRate); err != nil {
			return nil, invalid("Vorbis")
		}
	case CodecOpus:
		// OpusHead format and fields are validated by AddStream before this
		// codec-independent normalization point.
	case CodecVP8:
		// VP8 has no CodecPrivate.
		if len(data) != 0 {
			return nil, invalid("VP8")
		}
		return nil, nil
	case CodecVP9:
		// VP9 CodecPrivate is optional, but if supplied it uses Matroska's
		// feature-metadata TLV form rather than the MP4 vpcC record.
		if len(data) == 0 {
			return nil, nil
		}
		switch stream.Config.Format {
		case CodecConfigVP9FeatureMetadata:
			if err := vp9.ValidateFeatureMetadata(data); err != nil {
				return nil, invalid("VP9")
			}
		case CodecConfigVPCC:
			converted, err := vp9.FeatureMetadataFromVPCC(data)
			if err != nil {
				return nil, invalid("VP9")
			}
			return converted, nil
		default:
			return nil, fmt.Errorf("%w: VP9 requires feature metadata or vpcC", ErrUnsupportedCodec)
		}
	default:
		return nil, fmt.Errorf("%w: %s in EBML", ErrUnsupportedCodec, stream.Codec)
	}
	return append([]byte(nil), data...), nil
}

func (m *ebmlMuxer) codecAllowed(codec CodecID) bool {
	switch codec {
	case CodecVP8, CodecVP9, CodecAV1, CodecOpus, CodecVorbis:
		return true
	case CodecFLAC, CodecH264, CodecHEVC:
		return m.format == FormatMatroska
	default:
		return false
	}
}

func (m *ebmlMuxer) WritePacket(ctx context.Context, packet *Packet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrClosed
	}
	if packet == nil {
		return nil
	}
	if packet.StreamIndex < 0 || packet.StreamIndex >= len(m.streams) ||
		!packet.PTS.Valid || !packet.DTS.Valid || !packet.Duration.Valid {
		return ErrInvalidData
	}
	stream := m.streams[packet.StreamIndex]
	timecode, ok := stream.TimeBase.Rescale(packet.PTS.Value, Rational{Num: 1, Den: 1_000})
	if !ok || timecode < 0 {
		return fmt.Errorf("%w: EBML requires a non-negative presentation timestamp", ErrInvalidData)
	}
	duration, valid := stream.TimeBase.Rescale(packet.Duration.Value, Rational{Num: 1, Den: 1_000})
	if !valid || duration <= 0 || timecode > math.MaxInt64-duration {
		return fmt.Errorf("%w: invalid EBML packet duration", ErrInvalidData)
	}
	if !m.started {
		if err := m.writeHeader(); err != nil {
			return err
		}
		m.started = true
	}
	relative := timecode - m.clusterTime
	if m.cluster == nil || relative > 30_000 || relative < math.MinInt16 {
		if err := m.closeCluster(); err != nil {
			return err
		}
		cluster, err := webm.BeginCluster(m.w, m.w.seekable, uint64(timecode))
		if err != nil {
			return err
		}
		m.cluster = cluster
		m.clusterTime = timecode
		relative = 0
		if m.w.seekable {
			m.cues = append(m.cues, webm.CuePoint{
				Timecode:        uint64(timecode),
				Track:           uint64(packet.StreamIndex + 1),
				ClusterPosition: uint64(cluster.StartOffset() - m.header.SegmentStart),
			})
		}
	}
	if err := m.cluster.WriteBlockGroup(uint64(packet.StreamIndex+1), int16(relative), packet.Keyframe(),
		uint64(duration), int64(packet.DiscardPadding), packet.Data); err != nil {
		return err
	}
	end := timecode + duration
	if end > m.maxEndTime {
		m.maxEndTime = end
	}
	return nil
}

func (m *ebmlMuxer) writeHeader() error {
	docType := webm.DocTypeWebM
	if m.format == FormatMatroska {
		docType = webm.DocTypeMatroska
	}
	if err := webm.WriteEBMLHeaderFor(m.w, docType); err != nil {
		return err
	}
	header, err := webm.BeginSegment(m.w, m.w.seekable)
	if err != nil {
		return err
	}
	m.header = header
	if err := webm.WriteInfo(m.w, &m.header, ebmlTimecodeScale, time.Unix(978307200, 0).UTC()); err != nil {
		return err
	}
	m.header.TracksStart = m.w.offset
	if _, err := webm.WriteTracks(m.w, m.tracks); err != nil {
		return err
	}
	m.header.TracksEnd = m.w.offset
	return nil
}

func (m *ebmlMuxer) closeCluster() error {
	if m.cluster == nil {
		return nil
	}
	err := m.cluster.Close()
	m.cluster = nil
	return err
}

func (m *ebmlMuxer) Close() (retErr error) {
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	defer func() { m.closeErr = retErr }()
	if !m.started {
		return nil
	}
	if err := m.closeCluster(); err != nil {
		return err
	}
	if !m.w.seekable {
		return nil
	}
	if m.header.HasDuration {
		if err := webm.PatchDuration(m.w, m.header.DurationPayloadOff, float64(m.maxEndTime)); err != nil {
			return err
		}
	}
	cuesPosition := int64(-1)
	if len(m.cues) > 0 {
		position, err := webm.WriteCues(m.w, m.cues)
		if err != nil {
			return err
		}
		cuesPosition = position - m.header.SegmentStart
	}
	if err := webm.WriteSeekHead(m.w,
		m.header.InfoStart-m.header.SegmentStart,
		m.header.TracksStart-m.header.SegmentStart,
		cuesPosition); err != nil {
		return err
	}
	segmentLength := m.w.offset - m.header.SegmentStart
	if segmentLength < 0 {
		return fmt.Errorf("%w: invalid EBML segment length", ErrInvalidData)
	}
	return webm.PatchSegmentSize(m.w, m.header.SegmentSizeOff, 8, uint64(segmentLength))
}
