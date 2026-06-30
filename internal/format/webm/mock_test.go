package webm

import (
	"bytes"
	"errors"
	"io"
)

// mockSeeker is a bytes-based io.WriteSeeker for isolated unit tests.
type mockSeeker struct {
	buf []byte
	pos int
}

func newMockSeeker() *mockSeeker { return &mockSeeker{} }

func (m *mockSeeker) Write(p []byte) (int, error) {
	if m.pos+len(p) > len(m.buf) {
		grow := m.pos + len(p) - len(m.buf)
		m.buf = append(m.buf, make([]byte, grow)...)
	}
	copy(m.buf[m.pos:], p)
	m.pos += len(p)
	return len(p), nil
}

func (m *mockSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(m.pos) + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, errors.New("bad whence")
	}
	if abs < 0 || abs > int64(len(m.buf)) {
		return 0, errors.New("out of range")
	}
	m.pos = int(abs)
	return abs, nil
}

func (m *mockSeeker) Bytes() []byte { return m.buf }

// verify helper: ensure buf starts with the given prefix.
func assertPrefix(t mockTB, got, want []byte, label string) {
	t.Helper()
	if !bytes.HasPrefix(got, want) {
		t.Errorf("%s: got % X, want prefix % X", label, got[:min(len(got), len(want))], want)
	}
}

type mockTB interface {
	Helper()
	Errorf(format string, args ...any)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
