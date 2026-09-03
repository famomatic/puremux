package media

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

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
	adjust      map[int]int64
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
			info.Duration += segment.Duration
		}
		info.DurationKnown = true
	}
	return info
}

func (d *hlsDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	for {
		p, err := d.current.ReadPacket(ctx)
		if err == nil {
			if d.adjust == nil {
				d.adjust = make(map[int]int64)
			}
			stream := d.streams[p.StreamIndex]
			shift, exists := d.adjust[p.StreamIndex]
			if !exists {
				baseTicks, ok := nanosecondTimeBase.Rescale(d.baseNS, stream.TimeBase)
				if !ok {
					p.Release()
					return nil, ErrInvalidData
				}
				anchor := int64(0)
				if p.PTS.Valid {
					anchor = p.PTS.Value
				} else if p.DTS.Valid {
					anchor = p.DTS.Value
				}
				shift = baseTicks - anchor
				d.adjust[p.StreamIndex] = shift
			}
			if p.PTS.Valid {
				p.PTS.Value += shift
			}
			if p.DTS.Valid {
				p.DTS.Value += shift
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
		d.baseNS += int64(d.currentSeg.Duration)
		d.next++
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
}

func (d *hlsDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return SeekResult{}, ErrClosed
	}
	if !d.playlist.EndList {
		return SeekResult{}, ErrNotSeekable
	}
	targetNS := req.Target
	if req.StreamIndex >= 0 {
		if req.StreamIndex >= len(d.streams) {
			return SeekResult{}, errors.New("media: HLS stream index out of range")
		}
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
		end := start + int64(segment.Duration)
		if targetNS < end || i == len(d.segments)-1 {
			index = i
			break
		}
		start = end
	}
	_ = d.current.Close()
	d.baseNS, d.next = start, index
	if err := d.openSegment(ctx, index); err != nil {
		return SeekResult{}, err
	}
	localNS := targetNS - start
	_, _ = d.current.Seek(ctx, SeekRequest{StreamIndex: -1, Target: localNS})
	actual := start
	if req.StreamIndex >= 0 {
		actual, _ = nanosecondTimeBase.Rescale(actual, d.streams[req.StreamIndex].TimeBase)
	}
	return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
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
		body, err := d.fetch(ctx, rawURL, manifest.ByteRange{}, d.opts.MaxManifestBytes)
		if err != nil {
			return "", manifest.HLSPlaylist{}, err
		}
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
		data, err = d.decrypt(ctx, data, segment)
		if err != nil {
			return err
		}
	}
	var demuxer Demuxer
	if segment.Map != nil {
		cacheKey := segment.Map.URI + ":" + strconv.FormatInt(segment.Map.Range.Offset, 10) + ":" + strconv.FormatInt(segment.Map.Range.Length, 10)
		init := d.initCache[cacheKey]
		if init == nil {
			init, err = d.fetch(ctx, segment.Map.URI, segment.Map.Range, d.opts.MaxSegmentBytes)
			if err != nil {
				return err
			}
			if segment.Key != nil {
				if !segment.Key.HasIV {
					return errors.New("hls: encrypted EXT-X-MAP requires an explicit IV")
				}
				init, err = d.decrypt(ctx, init, segment)
				if err != nil {
					return err
				}
			}
			d.initCache[cacheKey] = append([]byte(nil), init...)
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
	d.current, d.currentSeg, d.adjust, d.firstPacket = demuxer, segment, nil, true
	return nil
}

func (d *hlsDemuxer) refresh(ctx context.Context) error {
	body, err := d.fetch(ctx, d.playlistURL, manifest.ByteRange{}, d.opts.MaxManifestBytes)
	if err != nil {
		return err
	}
	base, _ := url.Parse(d.playlistURL)
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
		return ErrNoNewSegments
	}
	d.playlist, d.segments, d.next = playlist, newSegments, 0
	return nil
}

func (d *hlsDemuxer) decrypt(ctx context.Context, encrypted []byte, segment manifest.HLSSegment) ([]byte, error) {
	key := d.keyCache[segment.Key.URI]
	if key == nil {
		var err error
		key, err = d.fetch(ctx, segment.Key.URI, manifest.ByteRange{}, 16)
		if err != nil {
			return nil, err
		}
		if len(key) != 16 {
			return nil, errors.New("hls: AES-128 key is not 16 bytes")
		}
		d.keyCache[segment.Key.URI] = append([]byte(nil), key...)
	}
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, errors.New("hls: AES-128 ciphertext is not block aligned")
	}
	iv := segment.Key.IV
	if !segment.Key.HasIV {
		binary.BigEndian.PutUint64(iv[8:], segment.Sequence)
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
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.root, cancel)
	defer func() { stop(); cancel() }()
	req, err := http.NewRequestWithContext(operation, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = d.header.Clone()
	req.Header.Set("Accept-Encoding", "identity")
	if byteRange.Valid {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteRange.Offset, byteRange.Offset+byteRange.Length-1))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if byteRange.Valid {
		if resp.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("hls: range request returned %s", resp.Status)
		}
		if !validHLSContentRange(resp.Header.Get("Content-Range"), byteRange.Offset, byteRange.Offset+byteRange.Length-1) {
			return nil, errors.New("hls: invalid Content-Range")
		}
		if length := resp.ContentLength; length >= 0 && length != byteRange.Length {
			return nil, errors.New("hls: byte-range length mismatch")
		}
		limit = min(limit, byteRange.Length)
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hls: HTTP request returned %s", resp.Status)
	}
	if limit <= 0 {
		return nil, errors.New("hls: invalid resource limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("hls: resource exceeds configured limit")
	}
	if byteRange.Valid && int64(len(data)) != byteRange.Length {
		return nil, errors.New("hls: truncated byte range")
	}
	return data, nil
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
		if a[i].Type != b[i].Type || a[i].Codec != b[i].Codec || a[i].TimeBase != b[i].TimeBase || a[i].SampleRate != b[i].SampleRate || a[i].Channels != b[i].Channels {
			return false
		}
	}
	return true
}

var _ Demuxer = (*hlsDemuxer)(nil)
