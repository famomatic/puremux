package media

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
)

// Source is the minimum sequential-input capability. A non-seekable Source
// can currently be opened as MPEG-TS using FormatHint or ProbeSequential;
// indexed formats require SeekableSource. Closing a Source must unblock an
// in-progress Read whenever the underlying implementation permits.
type Source interface {
	io.Reader
	io.Closer
	Name() string
}

// ContextSource supports per-operation cancellation without requiring Close.
type ContextSource interface {
	Source
	ReadContext(context.Context, []byte) (int, error)
}

// RandomAccessSource is the preferred capability for MP4 and indexed seeks.
type RandomAccessSource interface {
	Source
	io.ReaderAt
	Size() int64
}

type ContextRandomAccessSource interface {
	RandomAccessSource
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

type SeekableSource interface {
	Source
	io.Seeker
}

type fileSource struct {
	*os.File
	name string
	size int64
}

func OpenFile(path string) (RandomAccessSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &fileSource{File: f, name: path, size: st.Size()}, nil
}

func (s *fileSource) Name() string { return s.name }
func (s *fileSource) Size() int64  { return s.size }

func (s *fileSource) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.Read(p)
}

func (s *fileSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.ReadAt(p, off)
}

// MemorySource returns a seekable, random-access source over data. The byte
// slice is not copied and must remain immutable until the source is closed.
func MemorySource(name string, data []byte) RandomAccessSource {
	return &memorySource{name: name, r: bytes.NewReader(data), size: int64(len(data))}
}

type memorySource struct {
	mu     sync.Mutex
	name   string
	r      *bytes.Reader
	size   int64
	closed bool
}

func (s *memorySource) Name() string { return s.name }
func (s *memorySource) Size() int64  { return s.size }

func (s *memorySource) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.r.Read(p)
}

func (s *memorySource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.r.ReadAt(p, off)
}

func (s *memorySource) Seek(off int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.r.Seek(off, whence)
}

func (s *memorySource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memorySource) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.Read(p)
}

func (s *memorySource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.ReadAt(p, off)
}
