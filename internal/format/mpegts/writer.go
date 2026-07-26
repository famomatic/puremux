// Package mpegts implements a minimal MPEG-2 Transport Stream (ISO/IEC
// 13818-1) muxer for live streaming output.
//
// Scope: single-program TS carrying H.264/HEVC Annex-B video and ADTS AAC
// audio elementary streams. Every 188-byte packet is written as soon as it is
// produced, so a live HTTP download or player pipe receives data immediately
// (unlike fragmented-MP4 remuxing, which buffers whole fragments).
//
// The muxer upholds the ARCHITECTURE.md §5.C invariants: it assumes incoming
// packets are pristine and timecode-ordered (run them through the
// preprocessor first), never reorders or alters timestamps, and writes
// payloads opaque. Timestamps are only *converted*: time.Duration →  90 kHz
// PES ticks, rebased to the first packet plus a fixed headroom offset.
package mpegts

import (
	"errors"
	"io"
	"time"

	"github.com/famomatic/puremux/internal/core"
)

const (
	pktLen   = 188
	syncByte = 0x47

	pidPAT   = 0x0000
	pidPMT   = 0x1000
	pidFirst = 0x0100 // first elementary-stream PID; subsequent tracks increment

	// ptsHz is the PES/PCR base clock.
	ptsHz = 90000
	// ptsMask keeps PES timestamps within their 33-bit field.
	ptsMask = (uint64(1) << 33) - 1

	// ptsOffset (10s at 90 kHz) rebases the first packet away from zero so
	// slight cross-track lead (audio ahead of the first video keyframe after
	// preprocessing) can never underflow the unsigned PES field.
	ptsOffset = 10 * ptsHz

	// tablesInterval is the number of PES packets between PAT/PMT re-emissions
	// so a mid-stream reader can sync. At a typical 50fps+audio cadence this
	// is well under a second.
	tablesInterval = 40

	// MPEG-TS stream_type values (ISO/IEC 13818-1 table 2-34).
	streamTypeH264 = 0x1B
	streamTypeHEVC = 0x24
	streamTypeAAC  = 0x0F // ADTS-framed AAC (ISO/IEC 13818-7)
)

// ErrUnsupportedCodec is returned by AddTrack for codecs MPEG-TS cannot carry.
var ErrUnsupportedCodec = errors.New("mpegts: codec not supported in transport stream")

// ErrTrackAfterStart is returned when AddTrack is called after WritePacket.
var ErrTrackAfterStart = errors.New("mpegts: AddTrack after first WritePacket")

// ErrUnknownTrack is returned when a packet references an unregistered track.
var ErrUnknownTrack = errors.New("mpegts: unknown track")

type trackState struct {
	track      core.Track
	pid        uint16
	streamID   byte // PES stream_id: 0xE0.. video, 0xC0.. audio
	streamType byte // PMT stream_type
	cc         byte // continuity counter
}

// Muxer serializes ordered packets into a single-program transport stream.
// Not safe for concurrent use; drive it from one goroutine (§6 single-writer).
type Muxer struct {
	w       io.Writer
	tracks  []*trackState
	byID    map[int]*trackState
	pcrPID  uint16
	started bool

	haveBase bool
	base     time.Duration // DTS of the first written packet

	ccPAT, ccPMT byte
	pesSinceSI   int
}

// New creates a TS muxer writing to w. Register tracks with AddTrack, then
// feed monotonically ordered packets via WritePacket.
func New(w io.Writer) *Muxer {
	return &Muxer{w: w, byID: make(map[int]*trackState)}
}

// AddTrack registers a track before the first WritePacket. The returned ID
// equals t.ID when set (so callers can keep their own numbering), else a
// 1-based sequential number.
func (m *Muxer) AddTrack(t core.Track) (int, error) {
	if m.started {
		return 0, ErrTrackAfterStart
	}
	var st byte
	switch t.Codec {
	case core.CodecH264:
		st = streamTypeH264
	case core.CodecHEVC:
		st = streamTypeHEVC
	case core.CodecAAC:
		st = streamTypeAAC
	default:
		return 0, ErrUnsupportedCodec
	}
	id := t.ID
	if id == 0 {
		id = len(m.tracks) + 1
	}
	if _, dup := m.byID[id]; dup {
		return 0, errors.New("mpegts: duplicate track ID")
	}
	ts := &trackState{
		track:      t,
		pid:        uint16(pidFirst + len(m.tracks)),
		streamType: st,
	}
	nVid, nAud := 0, 0
	for _, other := range m.tracks {
		if other.track.Codec.IsVideo() {
			nVid++
		} else {
			nAud++
		}
	}
	if t.Codec.IsVideo() {
		ts.streamID = byte(0xE0 + nVid)
	} else {
		ts.streamID = byte(0xC0 + nAud)
	}
	m.tracks = append(m.tracks, ts)
	m.byID[id] = ts

	// PCR rides the first video track's PID, or the first track when the
	// stream is audio-only.
	m.pcrPID = 0
	for _, tr := range m.tracks {
		if tr.track.Codec.IsVideo() {
			m.pcrPID = tr.pid
			break
		}
	}
	if m.pcrPID == 0 {
		m.pcrPID = m.tracks[0].pid
	}
	return id, nil
}

// WritePacket serializes one elementary-stream packet (a full H.264/HEVC
// Annex-B access unit, or one ADTS AAC frame) as a PES packet. Packets MUST
// arrive in monotonic DTS order across all tracks (preprocessor output).
func (m *Muxer) WritePacket(p *core.Packet) error {
	if p == nil {
		return nil
	}
	ts, ok := m.byID[p.TrackID]
	if !ok {
		return ErrUnknownTrack
	}
	m.started = true
	if !m.haveBase {
		m.haveBase = true
		m.base = p.DTS
	}
	if err := m.emitTables(); err != nil {
		return err
	}
	pts := m.toTicks(p.PTS)
	dts := m.toTicks(p.DTS)
	withPCR := ts.pid == m.pcrPID
	return m.writePES(ts, pts, dts, withPCR, p.Data)
}

// Close finalizes the stream. MPEG-TS has no trailer; Close only exists to
// satisfy the muxer.Muxer contract.
func (m *Muxer) Close() error { return nil }

// toTicks converts a packet timestamp to 90 kHz ticks rebased to the first
// packet plus the headroom offset, masked to the 33-bit PES field. The
// conversion rounds to nearest so accumulated fractional-tick timestamps
// (e.g. 1024-sample AAC frames at 48 kHz = 1920.0 ticks but 21333333ns) do
// not drift a tick low under truncation.
func (m *Muxer) toTicks(d time.Duration) uint64 {
	rel := d - m.base
	ticks := int64(ptsOffset) + divRound(int64(rel)*ptsHz, int64(time.Second))
	// Clamp anything beyond the 10s headroom rather than wrapping negative.
	return uint64(max(ticks, 0)) & ptsMask
}

// divRound divides rounding half away from zero.
func divRound(n, d int64) int64 {
	if n >= 0 {
		return (n + d/2) / d
	}
	return (n - d/2) / d
}

// emitTables (re)emits PAT+PMT at the start and periodically.
func (m *Muxer) emitTables() error {
	if m.pesSinceSI == 0 || m.pesSinceSI >= tablesInterval {
		if err := m.writeSection(pidPAT, &m.ccPAT, buildPAT()); err != nil {
			return err
		}
		if err := m.writeSection(pidPMT, &m.ccPMT, m.buildPMT()); err != nil {
			return err
		}
		m.pesSinceSI = 0
	}
	m.pesSinceSI++
	return nil
}

// writeSection emits one PSI section in a single TS packet (PAT/PMT here are
// far below the 184-byte payload bound).
func (m *Muxer) writeSection(pid uint16, cc *byte, sec []byte) error {
	pkt := make([]byte, 0, pktLen)
	pkt = append(pkt, syncByte, byte(0x40|(pid>>8)), byte(pid&0xFF), 0x10|(*cc&0x0F))
	*cc = (*cc + 1) & 0x0F
	pkt = append(pkt, 0x00) // pointer_field
	pkt = append(pkt, sec...)
	for len(pkt) < pktLen {
		pkt = append(pkt, 0xFF)
	}
	_, err := m.w.Write(pkt[:pktLen])
	return err
}

// writePES packetizes one PES packet across TS packets. The first TS packet
// carries payload_unit_start plus (optionally) a PCR adaptation field; the
// final packet is stuffed via the adaptation field to exactly 188 bytes.
func (m *Muxer) writePES(ts *trackState, pts, dts uint64, withPCR bool, payload []byte) error {
	pes := append(pesHeader(ts.streamID, pts, dts, len(payload)), payload...)
	first := true
	for len(pes) > 0 || first {
		pkt := make([]byte, 4, pktLen)
		pkt[0] = syncByte
		pkt[1] = byte(ts.pid >> 8)
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
		}
		pkt[2] = byte(ts.pid & 0xFF)
		var af []byte
		if first && withPCR {
			// PCR from the packet DTS: 33-bit base + 6 reserved bits + 9-bit
			// extension (0). af = length, flags(PCR_flag), 6 PCR bytes.
			af = []byte{0x07, 0x10,
				byte(dts >> 25), byte(dts >> 17), byte(dts >> 9), byte(dts >> 1),
				byte((dts&1)<<7) | 0x7E, 0x00}
		}
		avail := pktLen - 4 - len(af)
		if len(pes) < avail {
			// Stuff the adaptation field so the packet is exactly 188 bytes.
			need := avail - len(pes)
			if len(af) == 0 {
				if need == 1 {
					af = []byte{0x00} // adaptation_field_length = 0
				} else {
					af = make([]byte, need)
					af[0] = byte(need - 1)
					af[1] = 0x00 // no flags
					for i := 2; i < need; i++ {
						af[i] = 0xFF
					}
				}
			} else {
				old := len(af)
				af = append(af, make([]byte, need)...)
				for i := old; i < len(af); i++ {
					af[i] = 0xFF
				}
				af[0] = byte(len(af) - 1)
			}
			avail = pktLen - 4 - len(af)
		}
		if len(af) > 0 {
			pkt[3] = 0x30 | (ts.cc & 0x0F) // adaptation + payload
		} else {
			pkt[3] = 0x10 | (ts.cc & 0x0F) // payload only
		}
		ts.cc = (ts.cc + 1) & 0x0F
		pkt = append(pkt, af...)
		take := min(avail, len(pes))
		pkt = append(pkt, pes[:take]...)
		pes = pes[take:]
		for len(pkt) < pktLen {
			pkt = append(pkt, 0xFF)
		}
		if _, err := m.w.Write(pkt[:pktLen]); err != nil {
			return err
		}
		first = false
	}
	return nil
}

// pesHeader builds a PES packet header. When pts != dts both are written
// (PTS_DTS_flags = 0b11); otherwise only the PTS (0b10). ISO/IEC 13818-1
// §2.4.3.7.
func pesHeader(streamID byte, pts, dts uint64, payloadLen int) []byte {
	h := []byte{0x00, 0x00, 0x01, streamID}
	var opt []byte
	if pts != dts {
		opt = make([]byte, 0, 13)
		opt = append(opt, 0x80, 0xC0, 0x0A) // flags: PTS+DTS, header len 10
		opt = appendTimestamp(opt, 0x30, pts)
		opt = appendTimestamp(opt, 0x10, dts)
	} else {
		opt = make([]byte, 0, 8)
		opt = append(opt, 0x80, 0x80, 0x05) // flags: PTS only, header len 5
		opt = appendTimestamp(opt, 0x20, pts)
	}
	total := len(opt) + payloadLen
	length := 0 // 0 = unbounded, allowed for video ES; simplest for large AUs
	if total <= 0xFFFF {
		length = total
	}
	h = append(h, byte(length>>8), byte(length))
	return append(h, opt...)
}

// appendTimestamp encodes a 33-bit PES timestamp in the 5-byte '...1 marker'
// format. prefix carries the 4-bit guard nibble ('0010' PTS-only, '0011' PTS
// of a PTS+DTS pair, '0001' DTS).
func appendTimestamp(dst []byte, prefix byte, v uint64) []byte {
	return append(dst,
		prefix|byte((v>>29)&0x0E)|0x01,
		byte(v>>22), byte((v>>14)&0xFE)|0x01,
		byte(v>>7), byte((v<<1)&0xFE)|0x01)
}

// buildPAT returns the PAT section: program 1 → PMT PID.
func buildPAT() []byte {
	sec := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, byte(0xE0 | (pidPMT >> 8)), byte(pidPMT & 0xFF)}
	return appendCRC(sec)
}

// buildPMT returns the PMT section listing every registered track.
func (m *Muxer) buildPMT() []byte {
	// section_length = bytes after it up to and including CRC:
	// 9 fixed + 5 per stream + 4 CRC.
	secLen := 9 + 5*len(m.tracks) + 4
	sec := []byte{0x02, byte(0xB0 | (secLen >> 8)), byte(secLen & 0xFF),
		0x00, 0x01, 0xC1, 0x00, 0x00,
		byte(0xE0 | (m.pcrPID >> 8)), byte(m.pcrPID & 0xFF), 0xF0, 0x00}
	for _, tr := range m.tracks {
		sec = append(sec, tr.streamType,
			byte(0xE0|(tr.pid>>8)), byte(tr.pid&0xFF), 0xF0, 0x00)
	}
	return appendCRC(sec)
}

func appendCRC(sec []byte) []byte {
	c := crc32MPEG(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

// crc32MPEG is the CRC-32/MPEG-2 used by PSI sections: poly 0x04C11DB7,
// init 0xFFFFFFFF, no reflection, no final XOR.
func crc32MPEG(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range data {
		crc ^= uint32(x) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
