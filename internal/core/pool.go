package core

import "sync"

// packetPool reuses *Packet values to keep allocation off the hot path
// (ARCHITECTURE.md section 5.A). The Data backing array is retained across
// reuses via Reset; callers MUST NOT hold references to a returned packet.
var packetPool = sync.Pool{
	New: func() any { return new(Packet) },
}

// AcquirePacket returns a reset *Packet from the pool.
func AcquirePacket() *Packet {
	p := packetPool.Get().(*Packet)
	p.Reset()
	return p
}

// ReleasePacket returns a packet to the pool. The packet and its Data slice
// must not be used after release.
func ReleasePacket(p *Packet) {
	if p == nil {
		return
	}
	packetPool.Put(p)
}