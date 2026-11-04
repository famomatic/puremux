package core

import "sync"

var smallPayloadPool = sync.Pool{New: func() any { return new([4096]byte) }}
var mediumPayloadPool = sync.Pool{New: func() any { return new([65536]byte) }}
var largePayloadPool = sync.Pool{New: func() any { return new([1048576]byte) }}

// AcquirePayload returns exclusive uninitialized storage for opaque bytes.
func AcquirePayload(n int) []byte {
	switch {
	case n <= 4096:
		return smallPayloadPool.Get().(*[4096]byte)[:n]
	case n <= 65536:
		return mediumPayloadPool.Get().(*[65536]byte)[:n]
	case n <= 1048576:
		return largePayloadPool.Get().(*[1048576]byte)[:n]
	default:
		return make([]byte, n)
	}
}

// ReleasePayload accepts only storage from AcquirePayload. Oversized buffers
// are discarded. Callers must retain the base address and release only once.
func ReleasePayload(data []byte) {
	switch cap(data) {
	case 4096:
		smallPayloadPool.Put((*[4096]byte)(data[:cap(data)]))
	case 65536:
		mediumPayloadPool.Put((*[65536]byte)(data[:cap(data)]))
	case 1048576:
		largePayloadPool.Put((*[1048576]byte)(data[:cap(data)]))
	}
}
