package mp4

import (
	"errors"
)

// initCursors primes the streaming cursors of every track so that peekNext can
// compute each track's first sample in O(1). It derives totalSamples from
// stsz/stts and positions the stts/chunk/stss cursors at the first sample.
//
// Unlike the old resolveTrack, it does NOT build a per-sample slice; memory
// stays O(tracks) regardless of how many samples the file carries.
func (rd *Reader) initCursors() error {
	for _, t := range rd.tracks {
		if err := t.initCursor(); err != nil {
			return err
		}
	}
	return nil
}

func (t *trackState) initCursor() error {
	// Total samples: prefer stsz count, else sum of stts counts.
	t.totalSamples = uint32(len(t.sampleSize))
	if t.uniform != 0 {
		var n uint32
		for _, e := range t.stts {
			n += e.count
		}
		if n > t.totalSamples {
			t.totalSamples = n
		}
	}
	if t.totalSamples == 0 {
		for _, e := range t.stts {
			t.totalSamples += e.count
		}
	}
	if t.totalSamples == 0 {
		return nil // empty track
	}
	// stts cursor at the first entry.
	if len(t.stts) > 0 {
		t.sttsIdx = 0
		t.sttsLeft = t.stts[0].count
	}
	t.accumUnits = 0
	// chunk cursor at the first chunk.
	if len(t.stco) > 0 {
		t.chunkIdx = 0
		t.chunkOff = int64(t.stco[0])
		t.chunkSampleLeft = samplesPerChunkFor(t.stsc, 1)
	}
	// stss cursor at the start.
	t.stssIdx = 0
	t.consumed = 0
	t.hasPeek = false
	return nil
}

// peekNext computes the next sample's metadata (time, offset, size, keyframe)
// without advancing the cursor. It returns false when the track is exhausted.
// It MUST leave the track state positioned so that advancePast can move past
// this sample.
func (t *trackState) peekNext() bool {
	if t.consumed >= t.totalSamples {
		return false
	}
	// Ensure the stts cursor covers sample number (consumed+1).
	for t.sttsIdx < len(t.stts) && t.sttsLeft == 0 {
		t.sttsIdx++
		if t.sttsIdx < len(t.stts) {
			t.sttsLeft = t.stts[t.sttsIdx].count
		}
	}
	absMs := timescaleToMs(t.accumUnits, t.timescale)

	// Size from stsz (uniform or per-sample).
	var size uint32
	if t.uniform != 0 {
		size = t.uniform
	} else if int(t.consumed) < len(t.sampleSize) {
		size = t.sampleSize[t.consumed]
	}

	// Offset: advance the chunk cursor until we land in a chunk that holds the
	// current sample. If the current chunk is exhausted, move to the next.
	for t.chunkSampleLeft == 0 && t.chunkIdx+1 < len(t.stco) {
		t.chunkIdx++
		t.chunkOff = int64(t.stco[t.chunkIdx])
		t.chunkSampleLeft = samplesPerChunkFor(t.stsc, uint32(t.chunkIdx+1))
	}
	off := t.chunkOff

	// Keyframe: stss lists 1-based sync sample numbers. Advance the stss
	// cursor past any sync numbers below the current sample, then check if the
	// current sample (consumed+1) matches the next sync entry.
	sampleNum := t.consumed + 1
	isKey := t.info.IsVideo
	if isKey && len(t.stss) > 0 {
		for t.stssIdx < len(t.stss) && t.stss[t.stssIdx] < sampleNum {
			t.stssIdx++
		}
		isKey = t.stssIdx < len(t.stss) && t.stss[t.stssIdx] == sampleNum
	} else if isKey && len(t.stss) == 0 {
		// No stss: every sample is a sync sample (all-intra). Audio is handled
		// by isKey being false (t.info.IsVideo == false).
		isKey = true
	} else {
		isKey = false
	}

	t.peek = samplePeek{
		absMs:    absMs,
		keyframe: isKey,
		off:      off,
		size:     size,
	}
	return true
}

// advancePast moves the cursor state past the peeked sample s, updating the
// stts time accumulator, the chunk offset, and the per-entry counts. The
// actual consumed counter is bumped by the caller.
func (t *trackState) advancePast(s samplePeek) {
	// stts: add this sample's delta to the accumulator.
	if t.sttsIdx < len(t.stts) {
		t.accumUnits += uint64(t.stts[t.sttsIdx].delta)
		if t.sttsLeft > 0 {
			t.sttsLeft--
		}
	}
	// chunk: consume one sample from the current chunk and advance the offset
	// by this sample's size.
	if t.chunkSampleLeft > 0 {
		t.chunkSampleLeft--
	}
	t.chunkOff += int64(s.size)
}

// ErrEmptyTrack is returned internally when a track has no samples; it is not
// surfaced (the track simply yields nothing).
var ErrEmptyTrack = errors.New("mp4: empty track")
