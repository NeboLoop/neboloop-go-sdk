package frame

import (
	"sync"
	"time"
)

const (
	dedupWindowSize = 1000
	dedupWindowTTL  = 5 * time.Minute
)

// dedupEntry tracks a seen message ID.
type dedupEntry struct {
	id   [16]byte
	seen time.Time
}

// DedupWindow is a per-conversation sliding window deduplicator.
// It remembers up to dedupWindowSize message IDs or dedupWindowTTL,
// whichever is reached first.
type DedupWindow struct {
	mu      sync.Mutex
	entries []dedupEntry
}

// NewDedupWindow creates a new dedup window.
func NewDedupWindow() *DedupWindow {
	return &DedupWindow{
		entries: make([]dedupEntry, 0, dedupWindowSize),
	}
}

// IsDuplicate returns true if the msg_id has already been seen.
// If not a duplicate, it records the ID.
func (d *DedupWindow) IsDuplicate(msgID [16]byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// Evict expired entries
	cutoff := now.Add(-dedupWindowTTL)
	start := 0
	for start < len(d.entries) && d.entries[start].seen.Before(cutoff) {
		start++
	}
	if start > 0 {
		d.entries = d.entries[start:]
	}

	// Check for duplicate
	for _, e := range d.entries {
		if e.id == msgID {
			return true
		}
	}

	// Evict oldest if at capacity
	if len(d.entries) >= dedupWindowSize {
		d.entries = d.entries[1:]
	}

	d.entries = append(d.entries, dedupEntry{id: msgID, seen: now})
	return false
}

// Len returns the current number of tracked IDs.
func (d *DedupWindow) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}
