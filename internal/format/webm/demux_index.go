package webm

import (
	"context"
	"errors"
	"io"
	"math"
	"time"
)

func (rd *DemuxReader) recordCluster(position int64, ticks uint64) {
	if rd.indexedClusters[position] {
		return
	}
	rd.indexedClusters[position] = true
	rd.clusters = append(rd.clusters, clusterEntry{position: position, timeTicks: ticks})
}

// consumeIndexElement collects trailing metadata during normal playback.
// Info/Tracks are fixed at Open; later Tracks cannot invalidate trackMap.
func (rd *DemuxReader) consumeIndexElement(e matroskaElement) error {
	switch e.ID {
	case idCues:
		return rd.parseCues(e)
	case idTags:
		return rd.parseTags(e)
	default:
		return rd.skipElement(e)
	}
}

// completeIndex is invoked only by explicit Seek. It does not change playback
// state or lose pending laced packets when canceled or when the source fails.
func (rd *DemuxReader) completeIndex(ctx context.Context) (retErr error) {
	saved, err := rd.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	defer func() { _, err := rd.rs.Seek(saved, io.SeekStart); retErr = errors.Join(retErr, err) }()
	start := rd.firstCluster
	if start < 0 {
		rd.indexComplete = true
		return nil
	}
	if _, err := rd.rs.Seek(start, io.SeekStart); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pos, err := rd.rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if pos >= rd.segmentEnd {
			rd.indexComplete = true
			return nil
		}
		e, err := rd.readElement()
		if errors.Is(err, io.EOF) {
			rd.indexComplete = true
			return nil
		}
		if err != nil {
			return err
		}
		if e.ID == idCluster {
			next, ticks, err := rd.scanClusterContext(ctx, e)
			if err != nil {
				return err
			}
			rd.recordCluster(e.start, ticks)
			if _, err := rd.rs.Seek(next, io.SeekStart); err != nil {
				return err
			}
		} else if err := rd.consumeIndexElement(e); err != nil {
			return err
		}
	}
}

func (rd *DemuxReader) setDuration(ticks float64) {
	ns := ticks * float64(rd.metadata.TimestampScaleNS)
	if ticks >= 0 && ns < float64(math.MaxInt64) {
		rd.metadata.Duration = time.Duration(ns)
		rd.metadata.DurationKnown = true
	}
}
