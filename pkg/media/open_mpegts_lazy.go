package media

import (
	"context"
	"errors"
	"io"

	"github.com/famomatic/puremux/internal/format/mpegts"
)

// A lazy seekable TS source retains the streaming parser until an explicit
// seek successfully builds a full index. Failed scans preserve parser state.
func openMPEGTSLazy(src Source, r io.Reader, seekable bool, streamContext *contextReader, seekContext *contextReadSeeker, lazy bool) (Demuxer, error) {
	d, err := openMPEGTS(src, r, seekable && !lazy, streamContext)
	if err != nil {
		return nil, err
	}
	if seekable && lazy {
		m := d.(*mpegTSDemuxer)
		m.lazyRS = r.(io.ReadSeeker)
		m.lazyContext = seekContext
		m.seekable = true
	}
	return d, nil
}
func (d *mpegTSDemuxer) indexLazy(ctx context.Context) (*mpegts.InputReader, error) {
	if d.lazyContext != nil {
		d.lazyContext.setContext(ctx)
	}
	pos, err := d.lazyRS.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err = d.lazyRS.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	candidate, scanErr := mpegts.NewInputReader(d.lazyRS)
	_, restoreErr := d.lazyRS.Seek(pos, io.SeekStart)
	if err = errors.Join(scanErr, restoreErr); err != nil {
		return nil, err
	}
	return candidate, nil
}
