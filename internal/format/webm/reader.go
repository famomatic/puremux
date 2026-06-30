package webm

import (
	"errors"
	"io"
	"time"

	"famomatic/puremux/internal/core"
	"famomatic/puremux/internal/format/ebml"
)

// Reader decodes a WebM/Matroska file into tracks and packets.
//
// It walks the EBML element tree, reading Tracks metadata and yielding
// SimpleBlock packets. Only header bytes are inspected to locate blocks;
// the compressed payloads are copied verbatim and remain opaque
// (ARCHITECTURE.md section 4).
type Reader struct {
	r        io.Reader
	tracks   []ReadTrack
	pos      int64
	segmentEnd int64 // -1 if unknown size
	pending  []*Block // blocks read from current cluster, awaiting yield
	curClusterEnd int64
}

// ReadTrack is a track parsed from the input WebM's Tracks element.
type ReadTrack struct {
	Number     int
	Codec      core.CodecType
	IsVideo    bool
	Width      int
	Height     int
	Channels   int
	SampleRate float64
	CodecPrivate []byte
}

// Block is a decoded SimpleBlock from the input.
type Block struct {
	TrackNum  int
	RelTimecode int16 // ms, relative to cluster
	Keyframe  bool
	Data      []byte
	ClusterTC uint64 // absolute cluster timecode (ms)
}

// NewReader wraps a WebM input stream.
func NewReader(r io.Reader) (*Reader, error) {
	rd := &Reader{r: r, segmentEnd: -1}
	if err := rd.parseHeader(); err != nil {
		return nil, err
	}
	return rd, nil
}

// Tracks returns the parsed track descriptors.
func (rd *Reader) Tracks() []ReadTrack { return rd.tracks }

// parseHeader reads the EBML header, then the Segment, and locates the
// Tracks element. After this, the reader is positioned at the first Cluster.
func (rd *Reader) parseHeader() error {
	// EBML header element.
	if err := rd.skipElement(); err != nil {
		return err
	}
	// Segment element.
	if _, err := rd.readChildHeader(); err != nil {
		return err
	}
	// Walk Segment children until we find Tracks, then stop (clusters follow).
	for {
		ch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		switch ch.ID {
		case idTracks:
			if err := rd.parseTracks(ch); err != nil {
				return err
			}
			return nil
		default:
			if err := rd.skipPayload(ch); err != nil {
				return err
			}
		}
	}
}

// NextBlock reads the next SimpleBlock from the stream. Returns io.EOF when
// the stream is exhausted. All SimpleBlocks within a Cluster are yielded in
// order before moving to the next Cluster.
func (rd *Reader) NextBlock() (*Block, error) {
	if len(rd.pending) > 0 {
		blk := rd.pending[0]
		rd.pending = rd.pending[1:]
		return blk, nil
	}
	for {
		ch, err := rd.readChildHeader()
		if err != nil {
			return nil, err
		}
		switch ch.ID {
		case idCluster:
			if err := rd.parseCluster(ch); err != nil {
				return nil, err
			}
			if len(rd.pending) > 0 {
				blk := rd.pending[0]
				rd.pending = rd.pending[1:]
				return blk, nil
			}
		default:
			if err := rd.skipPayload(ch); err != nil {
				return nil, err
			}
		}
	}
}

// parseCluster reads the cluster timestamp and all SimpleBlocks within,
// appending them to rd.pending for sequential yield by NextBlock.
func (rd *Reader) parseCluster(ch ebml.Header) error {
	clusterEnd := rd.elementEnd(ch)
	var clusterTC uint64
	for rd.pos < clusterEnd {
		cch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		switch cch.ID {
		case idTimestamp:
			tc, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			clusterTC = tc
		case idSimpleBlock:
			blk, err := rd.parseSimpleBlock(cch, clusterTC)
			if err != nil {
				return err
			}
			rd.pending = append(rd.pending, blk)
		case idBlockGroup:
			// BlockGroup may contain a Block (0xA1); parse it as a SimpleBlock
			// equivalent for our muxing purposes.
			blk, err := rd.parseBlockGroup(cch, clusterTC)
			if err != nil {
				return err
			}
			if blk != nil {
				rd.pending = append(rd.pending, blk)
			}
		default:
			if err := rd.skipPayload(cch); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseBlockGroup extracts the Block payload from a BlockGroup element.
func (rd *Reader) parseBlockGroup(ch ebml.Header, clusterTC uint64) (*Block, error) {
	end := rd.elementEnd(ch)
	for rd.pos < end {
		cch, err := rd.readChildHeader()
		if err != nil {
			return nil, err
		}
		switch cch.ID {
		case idBlock:
			return rd.parseSimpleBlock(cch, clusterTC)
		default:
			if err := rd.skipPayload(cch); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// parseSimpleBlock reads a SimpleBlock payload and decodes its header.
func (rd *Reader) parseSimpleBlock(ch ebml.Header, clusterTC uint64) (*Block, error) {
	payload := make([]byte, ch.Size)
	if _, err := io.ReadFull(rd.r, payload); err != nil {
		return nil, err
	}
	rd.pos += int64(len(payload))
	return decodeSimpleBlock(payload, clusterTC)
}

// decodeSimpleBlock parses the Block header (track VINT + int16 tc + flags).
func decodeSimpleBlock(payload []byte, clusterTC uint64) (*Block, error) {
	if len(payload) < 4 {
		return nil, errors.New("webm: SimpleBlock too short")
	}
	// Track number VINT (width 1 or 2).
	b0 := payload[0]
	w := ebml.VINTWidth(b0)
	if w == 0 || w > 2 {
		return nil, errors.New("webm: bad SimpleBlock track VINT")
	}
	var trackNum int
	if w == 1 {
		trackNum = int(b0 & 0x7F)
	} else {
		trackNum = int(b0&0x3F)<<8 | int(payload[1])
	}
	tc := int16(uint16(payload[w])<<8 | uint16(payload[w+1]))
	flags := payload[w+2]
	data := payload[w+3:]
	return &Block{
		TrackNum:    trackNum,
		RelTimecode: tc,
		Keyframe:    flags&0x80 != 0,
		Data:        data,
		ClusterTC:   clusterTC,
	}, nil
}

// --- EBML walking helpers ---

func (rd *Reader) readChildHeader() (ebml.Header, error) {
	h, err := ebml.ReadHeader(rd.r)
	if err != nil {
		return ebml.Header{}, err
	}
	// ReadHeader consumed ID bytes + size VINT bytes. Track both.
	idWidth := ebml.VINTWidth(0)
	// Determine ID width from the ID value (class 1..4).
	switch {
	case h.ID >= 0x81 && h.ID <= 0xFF:
		idWidth = 1
	case h.ID >= 0x4000 && h.ID <= 0x7FFF:
		idWidth = 2
	case h.ID >= 0x200000 && h.ID <= 0x3FFFFF:
		idWidth = 3
	case h.ID >= 0x10000000 && h.ID <= 0x1FFFFFFF:
		idWidth = 4
	}
	rd.pos += int64(idWidth) + int64(h.SizeWidth)
	return h, nil
}

func (rd *Reader) skipElement() error {
	h, err := rd.readChildHeader()
	if err != nil {
		return err
	}
	return rd.skipPayload(h)
}

func (rd *Reader) skipPayload(h ebml.Header) error {
	if h.Unknown {
		return errors.New("webm: cannot skip unknown-size element")
	}
	if _, err := io.CopyN(io.Discard, rd.r, int64(h.Size)); err != nil {
		return err
	}
	rd.pos += int64(h.Size)
	return nil
}

func (rd *Reader) elementEnd(h ebml.Header) int64 {
	return rd.pos + int64(h.Size)
}

func (rd *Reader) readUint(h ebml.Header) (uint64, error) {
	if h.Size == 0 || h.Size > 8 {
		return 0, errors.New("webm: bad uint size")
	}
	buf := make([]byte, h.Size)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return 0, err
	}
	rd.pos += int64(h.Size)
	var v uint64
	for _, b := range buf {
		v = (v << 8) | uint64(b)
	}
	return v, nil
}

// parseTracks reads the Tracks element children.
func (rd *Reader) parseTracks(ch ebml.Header) error {
	end := rd.elementEnd(ch)
	for rd.pos < end {
		cch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		if cch.ID == idTrackEntry {
			if err := rd.parseTrackEntry(cch); err != nil {
				return err
			}
		} else {
			if err := rd.skipPayload(cch); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseTrackEntry(ch ebml.Header) error {
	end := rd.elementEnd(ch)
	t := ReadTrack{}
	for rd.pos < end {
		cch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		switch cch.ID {
		case idTrackNumber:
			v, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			t.Number = int(v)
		case idTrackType:
			v, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			t.IsVideo = v == 1
		case idCodecID:
			buf := make([]byte, cch.Size)
			if _, err := io.ReadFull(rd.r, buf); err != nil {
				return err
			}
			rd.pos += int64(cch.Size)
			t.Codec = codecTypeFromString(string(buf))
		case idCodecPrivate:
			buf := make([]byte, cch.Size)
			if _, err := io.ReadFull(rd.r, buf); err != nil {
				return err
			}
			rd.pos += int64(cch.Size)
			t.CodecPrivate = buf
		case idVideo:
			if err := rd.parseVideo(cch, &t); err != nil {
				return err
			}
		case idAudio:
			if err := rd.parseAudio(cch, &t); err != nil {
				return err
			}
		default:
			if err := rd.skipPayload(cch); err != nil {
				return err
			}
		}
	}
	rd.tracks = append(rd.tracks, t)
	return nil
}

func (rd *Reader) parseVideo(ch ebml.Header, t *ReadTrack) error {
	end := rd.elementEnd(ch)
	for rd.pos < end {
		cch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		switch cch.ID {
		case idPixelWidth:
			v, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			t.Width = int(v)
		case idPixelHeight:
			v, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			t.Height = int(v)
		default:
			if err := rd.skipPayload(cch); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseAudio(ch ebml.Header, t *ReadTrack) error {
	end := rd.elementEnd(ch)
	for rd.pos < end {
		cch, err := rd.readChildHeader()
		if err != nil {
			return err
		}
		switch cch.ID {
		case idChannels:
			v, err := rd.readUint(cch)
			if err != nil {
				return err
			}
			t.Channels = int(v)
		default:
			if err := rd.skipPayload(cch); err != nil {
				return err
			}
		}
	}
	if t.Channels == 0 {
		t.Channels = 2
	}
	if t.SampleRate == 0 {
		t.SampleRate = 48000
	}
	return nil
}

func codecTypeFromString(s string) core.CodecType {
	switch s {
	case "V_VP8":
		return core.CodecVP8
	case "V_VP9":
		return core.CodecVP9
	case "V_AV1":
		return core.CodecAV1
	case "A_OPUS":
		return core.CodecOpus
	default:
		return core.CodecUnknown
	}
}

// countingWriter is a no-op Writer used to satisfy io.TeeReader typing.
type countingWriter struct{}

func (countingWriter) Write(p []byte) (int, error) { return len(p), nil }

// AbsTimecode returns the block's absolute timecode in milliseconds.
func (b *Block) AbsTimecode() uint64 {
	return b.ClusterTC + uint64(int(b.RelTimecode))
}

// Duration returns the block's absolute timecode as a Duration.
func (b *Block) Duration() time.Duration {
	return time.Duration(b.AbsTimecode()) * time.Millisecond
}