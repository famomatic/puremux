package webm

import (
	"io"
)

// ClusterWriter accumulates SimpleBlocks within a single Cluster element.
// A Cluster holds packets whose relative timecodes fit in int16 (max
// 32767ms at TimecodeScale 1_000_000). The muxer opens a new Cluster when
// the current one would overflow that range or exceed the configured
// target duration.
type ClusterWriter struct {
	w         writeSeeker
	startOff  int64 // offset of Cluster ID
	sizeOff   int64 // offset of reserved size VINT
	sizeWidth int   // width of reserved size VINT (0 if streaming/unknown)
	tc        uint64 // cluster absolute timecode (ms)
	open      bool
	firstBlk  bool
	streaming bool
}

// BeginCluster writes the Cluster ID + Timestamp and reserves a size VINT
// (seekable) or unknown sentinel (streaming). The cluster timecode is the
// absolute millisecond timecode of the first packet in the cluster.
func BeginCluster(ws writeSeeker, seekable bool, absTimecodeMs uint64) (*ClusterWriter, error) {
	cw := &ClusterWriter{
		w:         ws,
		sizeWidth: 0,
		tc:        absTimecodeMs,
		open:      true,
		firstBlk:  true,
		streaming: !seekable,
	}
	cw.startOff, _ = ws.Seek(0, io.SeekCurrent)
	if err := writeID(ws, idCluster); err != nil {
		return nil, err
	}
	cw.sizeOff, _ = ws.Seek(0, io.SeekCurrent)
	if seekable {
		// reserve 4-byte size (clusters are bounded; 4 bytes = 28 data bits).
		if err := writeSizeWidth(ws, 0, 4); err != nil {
			return nil, err
		}
		cw.sizeWidth = 4
	} else {
		// unknown-size sentinel width 4.
		b, _ := encodeUnknownSize(4)
		if _, err := ws.Write(b); err != nil {
			return nil, err
		}
	}
	// Timestamp element: cluster absolute timecode as uint.
	if err := writeUint(ws, idTimestamp, absTimecodeMs); err != nil {
		return nil, err
	}
	return cw, nil
}

// WriteSimpleBlock writes a 0xA3 SimpleBlock element into the cluster.
// relTimecode is the packet timecode relative to the cluster start (int16).
func (cw *ClusterWriter) WriteSimpleBlock(trackNum uint64, relTimecode int16, keyframe bool, payload []byte) error {
	if !cw.open {
		return io.ErrClosedPipe
	}
	blk := EncodeSimpleBlock(trackNum, relTimecode, keyframe, payload)
	return writeElement(cw.w, idSimpleBlock, blk)
}

// Close finalizes the cluster. For seekable sinks it patches the reserved
// size VINT with the actual cluster byte length.
func (cw *ClusterWriter) Close() error {
	if !cw.open {
		return nil
	}
	cw.open = false
	if cw.sizeWidth > 0 {
		end, err := cw.w.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		clusterLen := uint64(end - cw.startOff - 4 /*id*/ - int64(cw.sizeWidth))
		return patchSizeAt(cw.w, cw.sizeOff, cw.sizeWidth, clusterLen)
	}
	return nil
}

// Timecode returns the cluster's absolute timecode.
func (cw *ClusterWriter) Timecode() uint64 { return cw.tc }

// StartOffset returns the byte offset where this Cluster element begins.
func (cw *ClusterWriter) StartOffset() int64 { return cw.startOff }

// patchSizeAt seeks to off, writes the size VINT, and restores the position.
func patchSizeAt(ws writeSeeker, off int64, width int, size uint64) error {
	cur, err := ws.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := ws.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if err := writeSizeWidth(ws, size, width); err != nil {
		return err
	}
	_, err = ws.Seek(cur, io.SeekStart)
	return err
}
