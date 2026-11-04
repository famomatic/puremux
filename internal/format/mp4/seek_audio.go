package mp4

import "sort"

// Sparse checkpoints bound repeated constant-order audio seeks to one block
// of at most 1024 samples, without a full per-sample metadata expansion.
type audioSeekPoint struct {
	pts   int64
	state trackState
}

func (t *trackState) findAudioSeek(target int64, scale uint32, backward bool) (uint32, int64, error) {
	if len(t.audioSeek) == 0 {
		scan := *t
		scan.audioSeek = nil
		if err := scan.initCursor(); err != nil {
			return 0, 0, err
		}
		var checkpoints []audioSeekPoint
		for scan.consumed < scan.totalSamples {
			if !scan.peekNext() {
				if scan.cursorErr != nil {
					return 0, 0, scan.cursorErr
				}
				return 0, 0, ErrCorrupt
			}
			scan.hasPeek = true
			if scan.consumed%1024 == 0 {
				checkpoints = append(checkpoints, audioSeekPoint{pts: scan.peek.pts, state: scan})
			}
			scan.hasPeek = false
			scan.consumed++
			scan.advancePast(scan.peek)
		}
		if len(checkpoints) == 0 {
			return 0, 0, ErrCorrupt
		}
		t.audioSeek = checkpoints
	}
	cache := t.audioSeek
	i := sort.Search(len(cache), func(i int) bool { return !timestampLess(cache[i].pts, t.timescale, target, scale) })
	scan := cache[max(0, i-1)].state
	var chosen trackState
	found := false
	for scan.consumed < scan.totalSamples {
		if !scan.peekNext() {
			if scan.cursorErr != nil {
				return 0, 0, scan.cursorErr
			}
			return 0, 0, ErrCorrupt
		}
		scan.hasPeek = true
		if !backward {
			chosen = scan
			found = true
			if !timestampLess(scan.peek.pts, scan.timescale, target, scale) {
				break
			}
		} else {
			if timestampLess(target, scale, scan.peek.pts, scan.timescale) {
				if !found {
					chosen = scan
					found = true
				}
				break
			}
			chosen = scan
			found = true
		}
		scan.hasPeek = false
		scan.consumed++
		scan.advancePast(scan.peek)
	}
	if !found {
		return 0, 0, ErrCorrupt
	}
	*t = chosen
	t.audioSeek = cache
	return t.consumed, t.peek.pts, nil
}
