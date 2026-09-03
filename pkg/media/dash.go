package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/famomatic/puremux/internal/manifest"
)

type DASHRepresentation struct {
	ID                string
	MimeType          string
	ContentType       string
	Codecs            string
	Bandwidth         int64
	AudioSamplingRate int
}

type DASHOptions struct {
	Client               *http.Client
	Header               http.Header
	MaxManifestBytes     int64
	MaxSegmentBytes      int64
	MaxRepresentations   int
	MaxSegments          int
	SelectRepresentation func([]DASHRepresentation) int
}

type dashDemuxer struct {
	stateMu          sync.Mutex
	opMu             sync.Mutex
	client           *http.Client
	header           http.Header
	opts             DASHOptions
	representation   manifest.DASHRepresentation
	manifestDuration time.Duration
	dynamic          bool
	next             int
	current          Demuxer
	streams          []Stream
	adjust           map[int]int64
	init             []byte
	root             context.Context
	cancel           context.CancelFunc
	closed           bool
}

// OpenDASH opens an HTTP(S) MPD, selects one representation, and delegates
// its compressed segments to the normal demuxers. The default choice is the
// highest-bandwidth audio representation, falling back to all representations.
func OpenDASH(ctx context.Context, rawURL string, opts DASHOptions) (Demuxer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return nil, errors.New("dash: invalid HTTP(S) MPD URL")
	}
	if opts.MaxManifestBytes <= 0 {
		opts.MaxManifestBytes = 4 << 20
	}
	if opts.MaxSegmentBytes <= 0 {
		opts.MaxSegmentBytes = 128 << 20
	}
	if opts.MaxRepresentations <= 0 {
		opts.MaxRepresentations = 1000
	}
	if opts.MaxSegments <= 0 {
		opts.MaxSegments = 100000
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	header := opts.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	root, cancel := context.WithCancel(context.Background())
	d := &dashDemuxer{client: client, header: header, opts: opts, root: root, cancel: cancel}
	body, err := d.fetch(ctx, rawURL, manifest.ByteRange{}, opts.MaxManifestBytes)
	if err != nil {
		cancel()
		return nil, err
	}
	base, _ := url.Parse(rawURL)
	mpd, err := manifest.ParseDASH(base, body, opts.MaxRepresentations, opts.MaxSegments)
	if err != nil {
		cancel()
		return nil, err
	}
	index := selectDASHRepresentation(mpd.Representations, opts.SelectRepresentation)
	if index < 0 || index >= len(mpd.Representations) {
		cancel()
		return nil, errors.New("dash: representation selector returned an invalid index")
	}
	d.representation, d.manifestDuration, d.dynamic = mpd.Representations[index], mpd.Duration, mpd.Dynamic
	if d.representation.Initialization != nil && !d.representation.SingleFile {
		d.init, err = d.fetch(ctx, d.representation.Initialization.URI, d.representation.Initialization.Range, opts.MaxSegmentBytes)
		if err != nil {
			cancel()
			return nil, err
		}
	}
	if err := d.openSegment(ctx, 0); err != nil {
		cancel()
		return nil, err
	}
	return d, nil
}

func selectDASHRepresentation(representations []manifest.DASHRepresentation, selector func([]DASHRepresentation) int) int {
	public := make([]DASHRepresentation, len(representations))
	for i, rep := range representations {
		public[i] = DASHRepresentation{ID: rep.ID, MimeType: rep.MimeType, ContentType: rep.ContentType, Codecs: rep.Codecs, Bandwidth: rep.Bandwidth, AudioSamplingRate: rep.AudioSamplingRate}
	}
	if selector != nil {
		return selector(public)
	}
	best := -1
	for i, rep := range representations {
		audio := strings.EqualFold(rep.ContentType, "audio") || strings.HasPrefix(strings.ToLower(rep.MimeType), "audio/")
		if !audio {
			continue
		}
		if best < 0 || rep.Bandwidth > representations[best].Bandwidth {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	for i, rep := range representations {
		if best < 0 || rep.Bandwidth > representations[best].Bandwidth {
			best = i
		}
	}
	return best
}

func (d *dashDemuxer) Streams() []Stream {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	return cloneStreams(d.streams)
}

func (d *dashDemuxer) Info() Info {
	info := Info{Format: FormatDASH, FormatName: "dash", Duration: d.manifestDuration, DurationKnown: d.manifestDuration > 0 && !d.dynamic}
	return info
}

func (d *dashDemuxer) ReadPacket(ctx context.Context) (*Packet, error) {
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
				baseTicks, ok := nanosecondTimeBase.Rescale(int64(d.representation.Segments[d.next].Start), stream.TimeBase)
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
			return p, nil
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
		_ = d.current.Close()
		d.next++
		if d.next >= len(d.representation.Segments) {
			return nil, io.EOF
		}
		if err := d.openSegment(ctx, d.next); err != nil {
			return nil, err
		}
	}
}

func (d *dashDemuxer) Seek(ctx context.Context, req SeekRequest) (SeekResult, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stateMu.Lock()
	closed := d.closed
	d.stateMu.Unlock()
	if closed {
		return SeekResult{}, ErrClosed
	}
	if d.dynamic {
		return SeekResult{}, ErrNotSeekable
	}
	targetNS := req.Target
	if req.StreamIndex >= 0 {
		if req.StreamIndex >= len(d.streams) {
			return SeekResult{}, errors.New("media: DASH stream index out of range")
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
	index := len(d.representation.Segments) - 1
	for i, segment := range d.representation.Segments {
		if targetNS < int64(segment.Start+segment.Duration) {
			index = i
			break
		}
	}
	_ = d.current.Close()
	d.next = index
	if err := d.openSegment(ctx, index); err != nil {
		return SeekResult{}, err
	}
	start := int64(d.representation.Segments[index].Start)
	_, _ = d.current.Seek(ctx, SeekRequest{StreamIndex: -1, Target: targetNS - start})
	actual := start
	if req.StreamIndex >= 0 {
		actual, _ = nanosecondTimeBase.Rescale(actual, d.streams[req.StreamIndex].TimeBase)
	}
	return SeekResult{StreamIndex: req.StreamIndex, Timestamp: actual}, nil
}

func (d *dashDemuxer) Close() error {
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
	if d.current != nil {
		return d.current.Close()
	}
	return nil
}

func (d *dashDemuxer) openSegment(ctx context.Context, index int) error {
	if index < 0 || index >= len(d.representation.Segments) {
		return io.EOF
	}
	segment := d.representation.Segments[index]
	var demuxer Demuxer
	var err error
	if d.representation.SingleFile {
		source, openErr := OpenHTTP(ctx, segment.Resource.URI, HTTPSourceOptions{Client: d.client, Header: d.header})
		if openErr == nil {
			demuxer, err = Open(ctx, source, OpenOptions{})
			if err != nil {
				_ = source.Close()
			}
		} else if errors.Is(openErr, ErrNotSeekable) {
			var data []byte
			data, err = d.fetch(ctx, segment.Resource.URI, manifest.ByteRange{}, d.opts.MaxSegmentBytes)
			if err == nil {
				demuxer, err = Open(ctx, MemorySource(segment.Resource.URI, data), OpenOptions{})
			}
		} else {
			err = openErr
		}
	} else {
		var data []byte
		data, err = d.fetch(ctx, segment.Resource.URI, segment.Resource.Range, d.opts.MaxSegmentBytes)
		if err == nil && len(d.init) > 0 {
			demuxer, err = OpenMP4WithInit(ctx, d.init, MemorySource(segment.Resource.URI, data))
		} else if err == nil {
			demuxer, err = Open(ctx, MemorySource(segment.Resource.URI, data), OpenOptions{})
		}
	}
	if err != nil {
		return err
	}
	streams := demuxer.Streams()
	if d.streams == nil {
		d.streams = streams
	} else if !compatibleStreams(d.streams, streams) {
		_ = demuxer.Close()
		return errors.New("dash: representation stream configuration changed")
	}
	d.current, d.adjust = demuxer, nil
	return nil
}

func (d *dashDemuxer) fetch(ctx context.Context, rawURL string, byteRange manifest.ByteRange, limit int64) ([]byte, error) {
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
		if resp.StatusCode != http.StatusPartialContent || !validHLSContentRange(resp.Header.Get("Content-Range"), byteRange.Offset, byteRange.Offset+byteRange.Length-1) {
			return nil, errors.New("dash: invalid byte-range response")
		}
		limit = min(limit, byteRange.Length)
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("dash: HTTP request returned %s", resp.Status)
	}
	if limit <= 0 {
		return nil, errors.New("dash: invalid resource limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("dash: resource exceeds configured limit")
	}
	if byteRange.Valid && int64(len(data)) != byteRange.Length {
		return nil, errors.New("dash: truncated byte range")
	}
	return data, nil
}

var _ Demuxer = (*dashDemuxer)(nil)
