package frame

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// ULIDGen generates monotonic ULIDs (16 bytes each).
// Thread-safe via mutex. Entropy from crypto/rand.
type ULIDGen struct {
	mu   sync.Mutex
	last [16]byte
}

// NewULIDGen creates a new ULID generator.
func NewULIDGen() *ULIDGen {
	return &ULIDGen{}
}

// Next returns a new monotonic ULID as a 16-byte array.
//
// Layout (Crockford ULID spec):
//
//	[0-5]   48-bit Unix millisecond timestamp (big-endian)
//	[6-15]  80-bit random, monotonically incrementing within same ms
func (g *ULIDGen) Next() [16]byte {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := uint64(time.Now().UnixMilli())

	var id [16]byte
	// Timestamp: 6 bytes big-endian
	id[0] = byte(now >> 40)
	id[1] = byte(now >> 32)
	id[2] = byte(now >> 24)
	id[3] = byte(now >> 16)
	id[4] = byte(now >> 8)
	id[5] = byte(now)

	// Check if same millisecond as last — increment random part
	sameMs := id[0] == g.last[0] && id[1] == g.last[1] && id[2] == g.last[2] &&
		id[3] == g.last[3] && id[4] == g.last[4] && id[5] == g.last[5]

	if sameMs {
		// Copy previous random part and increment
		copy(id[6:], g.last[6:])
		// Increment the 80-bit random part (as big-endian)
		for i := 15; i >= 6; i-- {
			id[i]++
			if id[i] != 0 {
				break
			}
		}
	} else {
		// New millisecond: fresh random bytes
		rand.Read(id[6:])
	}

	g.last = id
	return id
}

// Timestamp extracts the millisecond timestamp from a ULID.
func Timestamp(id [16]byte) time.Time {
	ms := uint64(id[0])<<40 | uint64(id[1])<<32 | uint64(id[2])<<24 |
		uint64(id[3])<<16 | uint64(id[4])<<8 | uint64(id[5])
	return time.UnixMilli(int64(ms))
}

// ULIDToUint64 extracts a uint64 from the first 8 bytes for comparison/sorting.
func ULIDToUint64(id [16]byte) uint64 {
	return binary.BigEndian.Uint64(id[:8])
}
