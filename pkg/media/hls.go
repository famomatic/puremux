package media

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/manifest"
)

var ErrNoNewSegments = errors.New("media: live manifest has no new segments")

type HLSVariant struct {
	URI        string
	Bandwidth  int64
	Codecs     string
	AudioGroup string
}

type HLSOptions struct {
	Client           *http.Client
	Header           http.Header
	MaxManifestBytes int64
	MaxSegmentBytes  int64
	MaxEntries       int
	// SelectVariant returns a variant index. The default selects the highest
	// bandwidth variant, then prefers its DEFAULT audio rendition when one is
	// explicitly referenced by an AUDIO group.
	SelectVariant func([]HLSVariant) int
}

type hlsDemuxer struct {
	stateMu     sync.Mutex
	opMu        sync.Mutex
	client      *http.Client
	header      http.Header
	opts        HLSOptions
	playlistURL string
	playlist    manifest.HLSPlaylist
	segments    []manifest.HLSSegment
	next        int
	current     Demuxer
	currentSeg  manifest.HLSSegment
	streams     []Stream
	shiftNS     int64
	shiftSet    bool
	baseNS      int64
	firstPacket bool
	initCache   map[string][]byte
	keyCache    map[string][]byte
	root        context.Context
	cancel      context.CancelFunc
	closed      bool
}

// OpenHLS opens an HTTP(S) HLS master or media playlist. Segments are fetched
// lazily and delegated to the ordinary compressed-media demuxers.
func OpenHLS(ctx context.Context, rawURL string, opts HLSOptions) (Demuxer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return nil, fmt.Errorf("hls: invalid URL: %w", err)
	}
	if opts.MaxManifestBytes <= 0 {
		opts.MaxManifestBytes = 2 << 20
	}
	if opts.MaxSegmentBytes <= 0 {
		opts.MaxSegmentBytes = 128 << 20
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 100000
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	root, cancel := context.WithCancel(context.Background())
	header := opts.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	d := &hlsDemuxer{client: client, header: header, opts: opts, playlistURL: rawURL, initCache: make(map[string][]byte), keyCache: make(map[string][]byte), root: root, cancel: cancel}
	playlistURL, playlist, err := d.resolvePlaylist(ctx, rawURL)
	if err != nil {
		cancel()
		return nil, err
	}
	d.playlistURL, d.playlist, d.segments = playlistURL, playlist, playlist.Segments
	if err := d.openSegment(ctx, 0); err != nil {
		cancel()
		return nil, err
	}
	return d, nil
}

func (d *hlsDemuxer) Streams() []Stream {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	return cloneStreams(d.streams)
}

func (d *hlsDemuxer) Info() Info {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	info := Info{Format: FormatHLS, FormatName: "hls"}
	if d.playlist.EndList {
		for _, segment := range d.segments {
			if segment.Duration > 0 && info.Duration > time.Duration(math.MaxInt64)-segment.Duration {
				return info
			}
			info.Duration += segment.Duration
		}
		info.DurationKnown = true
	}
	return info
}

func (d *hlsDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	ctx, cleanup := manifestOperationContext(ctx, d.root)
	defer cleanup()
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	for {
		if d.current == nil {
			if d.next >= len(d.segments) {
				if d.playlist.EndList {
					return nil, io.EOF
				}
				if err := d.refresh(ctx); err != nil {
					return nil, err
				}
			}
			if err := d.openSegment(ctx, d.next); err != nil {
				return nil, err
			}
		}
		p, err := d.current.ReadPacket(ctx)
		if err == nil {
			if p.StreamIndex < 0 || p.StreamIndex >= len(d.streams) {
				p.Release()
				return nil, ErrInvalidData
			}
			stream := d.streams[p.StreamIndex]
			if !d.shiftSet {
				var err error
				d.shiftNS, err = segmentTimestampShift(p, stream.TimeBase, d.baseNS)
				if err != nil {
					p.Release()
					return nil, err
				}
				d.shiftSet = true
			}
			if err := shiftPacketTimestamps(p, stream.TimeBase, d.shiftNS); err != nil {
				p.Release()
				return nil, err
			}
			if d.firstPacket && d.currentSeg.Discontinuity {
				p.Flags |= PacketDiscontinuity
			}
			d.firstPacket = false
			return p, nil
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
		_ = d.current.Close()
		d.current = nil
		var ok bool
		d.baseNS, ok = checkedAddInt64(d.baseNS, int64(d.currentSeg.Duration))
		if !ok {
			return nil, ErrInvalidData
		}
		d.next++
	}
}

func (d *hlsDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	ctx, cleanup := manifestOperationContext(ctx, d.root)
	defer cleanup()
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return SeekResult{}, ErrClosed
	}
	if err := validateSeekRequest(req, len(d.streams)); err != nil {
		return SeekResult{}, err
	}
	if !d.playlist.EndList {
		return SeekResult{}, ErrNotSeekable
	}
	targetNS := req.Target
	if req.StreamIndex >= 0 {
		var ok bool
		targetNS, ok = d.streams[req.StreamIndex].TimeBase.Rescale(req.Target, nanosecondTimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	if targetNS < 0 {
		targetNS = 0
	}
	index, start := 0, int64(0)
	for i, segment := range d.segments {
		end, ok := checkedAddInt64(start, int64(segment.Duration))
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
		if targetNS < end || i == len(d.segments)-1 {
			index = i
			break
		}
		start = end
	}
	if d.current != nil {
		_ = d.current.Close()
		d.current = nil
	}
	d.baseNS, d.next = start, index
	if err := d.openSegment(ctx, index); err != nil {
		return SeekResult{}, err
	}
	if err := d.primeSegmentShift(ctx); err != nil {
		return SeekResult{}, err
	}
	localTarget, ok := checkedSubInt64(targetNS, d.shiftNS)
	if !ok {
		return SeekResult{}, ErrInvalidData
	}
	localRequest := SeekRequest{StreamIndex: -1, Target: localTarget, Flags: req.Flags}
	segmentStartTicks := int64(0)
	if req.StreamIndex >= 0 {
		var ok bool
		segmentStartTicks, ok = nanosecondTimeBase.Rescale(d.shiftNS, d.streams[req.StreamIndex].TimeBase)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
		localRequest.StreamIndex = req.StreamIndex
		localRequest.Target, ok = checkedSubInt64(req.Target, segmentStartTicks)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
	}
	localResult, err := d.current.Seek(ctx, localRequest)
	if err != nil {
		return SeekResult{}, err
	}
	if req.StreamIndex >= 0 {
		actual, ok := checkedAddInt64(segmentStartTicks, localResult.Timestamp)
		if !ok {
			return SeekResult{}, ErrInvalidData
		}
		return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
	}
	actual, ok := checkedAddInt64(d.shiftNS, localResult.Timestamp)
	if !ok {
		return SeekResult{}, ErrInvalidData
	}
	return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
}

func (d *hlsDemuxer) primeSegmentShift(ctx context.Context) error {
	p, err := d.current.ReadPacket(ctx)
	if err != nil {
		return err
	}
	defer p.Release()
	if p.StreamIndex < 0 || p.StreamIndex >= len(d.streams) {
		return ErrInvalidData
	}
	d.shiftNS, err = segmentTimestampShift(p, d.streams[p.StreamIndex].TimeBase, d.baseNS)
	if err == nil {
		d.shiftSet = true
	}
	return err
}

func (d *hlsDemuxer) Close() error {
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.cancel()
	d.stateMu.Unlock()
	d.opMu.Lock()
	defer d.opMu.Unlock()
	current := d.current
	if current != nil {
		return current.Close()
	}
	return nil
}

func (d *hlsDemuxer) resolvePlaylist(ctx context.Context, rawURL string) (string, manifest.HLSPlaylist, error) {
	seen := make(map[string]bool)
	for depth := 0; depth < 5; depth++ {
		if seen[rawURL] {
			return "", manifest.HLSPlaylist{}, errors.New("hls: playlist cycle")
		}
		seen[rawURL] = true
		body, finalURL, err := d.fetchManifest(ctx, rawURL, d.opts.MaxManifestBytes)
		if err != nil {
			return "", manifest.HLSPlaylist{}, err
		}
		rawURL = finalURL
		base, _ := url.Parse(rawURL)
		playlist, err := manifest.ParseHLS(base, body, d.opts.MaxEntries)
		if err != nil {
			return "", manifest.HLSPlaylist{}, err
		}
		if !playlist.Master {
			return rawURL, playlist, nil
		}
		index := -1
		variants := make([]HLSVariant, len(playlist.Variants))
		for i, variant := range playlist.Variants {
			variants[i] = HLSVariant{URI: variant.URI, Bandwidth: variant.Bandwidth, Codecs: variant.Codecs, AudioGroup: variant.AudioGroup}
			if index < 0 || variant.Bandwidth > playlist.Variants[index].Bandwidth {
				index = i
			}
		}
		if d.opts.SelectVariant != nil {
			index = d.opts.SelectVariant(variants)
		}
		if index < 0 || index >= len(playlist.Variants) {
			return "", manifest.HLSPlaylist{}, errors.New("hls: variant selector returned an invalid index")
		}
		selected := playlist.Variants[index]
		rawURL = selected.URI
		if selected.AudioGroup != "" {
			fallback := ""
			for _, rendition := range playlist.Renditions {
				if rendition.Type == "AUDIO" && rendition.GroupID == selected.AudioGroup && rendition.URI != "" {
					if fallback == "" {
						fallback = rendition.URI
					}
					if rendition.Default {
						fallback = rendition.URI
						break
					}
				}
			}
			if fallback != "" {
				rawURL = fallback
			}
		}
	}
	return "", manifest.HLSPlaylist{}, errors.New("hls: too many nested master playlists")
}

func (d *hlsDemuxer) openSegment(ctx context.Context, index int) error {
	if index < 0 || index >= len(d.segments) {
		return io.EOF
	}
	segment := d.segments[index]
	data, err := d.fetch(ctx, segment.URI, segment.Range, d.opts.MaxSegmentBytes)
	if err != nil {
		return err
	}
	if segment.Key != nil {
		data, err = d.decrypt(ctx, data, segment.Key, segment.Sequence)
		if err != nil {
			return err
		}
	}
	var demuxer Demuxer
	if segment.Map != nil {
		cacheKey := segment.Map.URI + ":" + strconv.FormatInt(segment.Map.Range.Offset, 10) + ":" + strconv.FormatInt(segment.Map.Range.Length, 10)
		if segment.Map.Key != nil {
			cacheKey += ":" + segment.Map.Key.URI + ":" + fmt.Sprintf("%x", segment.Map.Key.IV)
		}
		init := d.initCache[cacheKey]
		if init == nil {
			init, err = d.fetch(ctx, segment.Map.URI, segment.Map.Range, d.opts.MaxSegmentBytes)
			if err != nil {
				return err
			}
			if segment.Map.Key != nil {
				if !segment.Map.Key.HasIV {
					return errors.New("hls: encrypted EXT-X-MAP requires an explicit IV")
				}
				init, err = d.decrypt(ctx, init, segment.Map.Key, segment.Sequence)
				if err != nil {
					return err
				}
			}
			// Keep only the active initialization; rotating maps cannot grow the cache.
			clear(d.initCache)
			d.initCache[cacheKey] = init
		}
		demuxer, err = OpenMP4WithInit(ctx, init, MemorySource(segment.URI, data))
	} else {
		demuxer, err = Open(ctx, MemorySource(segment.URI, data), OpenOptions{})
	}
	if err != nil {
		return err
	}
	streams := demuxer.Streams()
	if d.streams == nil {
		d.streams = streams
	} else if !compatibleStreams(d.streams, streams) {
		_ = demuxer.Close()
		return errors.New("hls: stream configuration changed without a supported transition")
	}
	d.current, d.currentSeg, d.shiftNS, d.shiftSet, d.firstPacket = demuxer, segment, 0, false, true
	return nil
}

func (d *hlsDemuxer) refresh(ctx context.Context) error {
	body, finalURL, err := d.fetchManifest(ctx, d.playlistURL, d.opts.MaxManifestBytes)
	if err != nil {
		return err
	}
	d.playlistURL = finalURL
	base, _ := url.Parse(finalURL)
	playlist, err := manifest.ParseHLS(base, body, d.opts.MaxEntries)
	if err != nil {
		return err
	}
	nextSequence := d.currentSeg.Sequence + 1
	var newSegments []manifest.HLSSegment
	for _, segment := range playlist.Segments {
		if segment.Sequence >= nextSequence {
			newSegments = append(newSegments, segment)
		}
	}
	if len(newSegments) == 0 {
		d.playlist = playlist
		if playlist.EndList {
			return io.EOF
		}
		return &LiveWaitError{Delay: max(250*time.Millisecond, playlist.TargetDuration/2)}
	}
	d.playlist, d.segments, d.next = playlist, newSegments, 0
	return nil
}

func (d *hlsDemuxer) decrypt(ctx context.Context, encrypted []byte, keySpec *manifest.HLSKey, sequence uint64) ([]byte, error) {
	key, err := d.fetch(ctx, keySpec.URI, manifest.ByteRange{}, 16)
	if err != nil {
		return nil, err
	}
	if len(key) != 16 {
		return nil, errors.New("hls: AES-128 key is not 16 bytes")
	}
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, errors.New("hls: AES-128 ciphertext is not block aligned")
	}
	iv := keySpec.IV
	if !keySpec.HasIV {
		binary.BigEndian.PutUint64(iv[8:], sequence)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), encrypted...)
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(out, out)
	padding := int(out[len(out)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(out) {
		return nil, errors.New("hls: invalid AES-128 padding")
	}
	for _, value := range out[len(out)-padding:] {
		if int(value) != padding {
			return nil, errors.New("hls: invalid AES-128 padding")
		}
	}
	return out[:len(out)-padding], nil
}

func (d *hlsDemuxer) fetch(ctx context.Context, rawURL string, byteRange manifest.ByteRange, limit int64) ([]byte, error) {
	data, _, err := fetchManifestResource(ctx, d.root, d.client, d.header, rawURL, byteRange, limit)
	return data, err
}

func (d *hlsDemuxer) fetchManifest(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	return fetchManifestResource(ctx, d.root, d.client, d.header, rawURL, manifest.ByteRange{}, limit)
}

func validHLSContentRange(value string, wantStart, wantEnd int64) bool {
	if !strings.HasPrefix(value, "bytes ") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 || parts[1] == "" {
		return false
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return false
	}
	start, errStart := strconv.ParseInt(bounds[0], 10, 64)
	end, errEnd := strconv.ParseInt(bounds[1], 10, 64)
	if errStart != nil || errEnd != nil || start != wantStart || end != wantEnd {
		return false
	}
	if parts[1] == "*" {
		return true
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && total > end
}

func compatibleStreams(a, b []Stream) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Codec != b[i].Codec || a[i].TimeBase != b[i].TimeBase || a[i].SampleRate != b[i].SampleRate || a[i].Channels != b[i].Channels || a[i].Width != b[i].Width || a[i].Height != b[i].Height || a[i].Config.Format != b[i].Config.Format || !bytes.Equal(a[i].Config.Data, b[i].Config.Data) {
			return false
		}
	}
	return true
}

func checkedAddInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSubInt64(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		if a >= 0 {
			return 0, false
		}
		return a - b, true
	}
	return checkedAddInt64(a, -b)
}

func shiftPacketTimestamps(packet *Packet, timeBase Rational, shiftNS int64) error {
	shift, ok := nanosecondTimeBase.Rescale(shiftNS, timeBase)
	if !ok {
		return ErrInvalidData
	}
	if packet.PTS.Valid {
		adjusted, ok := checkedAddInt64(packet.PTS.Value, shift)
		if !ok {
			return ErrInvalidData
		}
		packet.PTS.Value = adjusted
	}
	if packet.DTS.Valid {
		adjusted, ok := checkedAddInt64(packet.DTS.Value, shift)
		if !ok {
			return ErrInvalidData
		}
		packet.DTS.Value = adjusted
	}
	return nil
}

func segmentTimestampShift(packet *Packet, timeBase Rational, baseNS int64) (int64, error) {
	anchor := int64(0)
	if packet.PTS.Valid {
		anchor = packet.PTS.Value
	} else if packet.DTS.Valid {
		anchor = packet.DTS.Value
	}
	anchorNS, ok := timeBase.Rescale(anchor, nanosecondTimeBase)
	if !ok {
		return 0, ErrInvalidData
	}
	shift, ok := checkedSubInt64(baseNS, anchorNS)
	if !ok {
		return 0, ErrInvalidData
	}
	return shift, nil
}

var _ Demuxer = (*hlsDemuxer)(nil)
