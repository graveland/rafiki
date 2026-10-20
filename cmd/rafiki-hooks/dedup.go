// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"
)

const (
	// dedupTTL is how long a delivered id is remembered. A sender's retry
	// window is minutes, not days; a day of slack costs little and covers a
	// sender dribbling out retries.
	dedupTTL = 24 * time.Hour
	// dedupCap bounds the map. At the cap the oldest entry is evicted, so a
	// sender that fabricates delivery ids on every request can push real ones
	// out but cannot grow the gateway's memory.
	dedupCap = 10000
)

// dedup remembers which deliveries have been delivered, so a sender's retry
// after a lost response does not wake an agent twice.
//
// An empty id is never deduped: a sender that sends none gets every request
// through, which is the only safe reading of "no id".
type dedup struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
	max     int
}

func newDedup() *dedup {
	return &dedup{
		entries: make(map[string]time.Time),
		ttl:     dedupTTL,
		max:     dedupCap,
	}
}

// seen records id as delivered at now and reports whether it was already
// present within the TTL.
func (d *dedup) seen(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live(id, now) {
		return true
	}
	d.recordLocked(id, now)
	return false
}

// contains reports whether id was already delivered, without recording it.
//
// The handler uses contains and record rather than seen, because a delivery is
// only "seen" once Notify has answered OK: an id recorded before the call would
// turn the sender's retry of a 503 into a duplicate, and the notification would
// be lost.
func (d *dedup) contains(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.live(id, now)
}

// record marks id as delivered at now.
func (d *dedup) record(id string, now time.Time) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recordLocked(id, now)
}

// live reports whether id is present and unexpired, dropping it if it expired.
// The caller holds the mutex.
func (d *dedup) live(id string, now time.Time) bool {
	at, ok := d.entries[id]
	if !ok {
		return false
	}
	if now.Sub(at) >= d.ttl {
		delete(d.entries, id)
		return false
	}
	return true
}

// recordLocked stores id and keeps the map within its cap. The caller holds the
// mutex.
func (d *dedup) recordLocked(id string, now time.Time) {
	if len(d.entries) >= d.max {
		for k, at := range d.entries {
			if now.Sub(at) >= d.ttl {
				delete(d.entries, k)
			}
		}
	}
	d.entries[id] = now
	for len(d.entries) > d.max {
		oldestID := ""
		var oldestAt time.Time
		for k, at := range d.entries {
			if k == id {
				// Never evict the entry just recorded: at the cap with equal
				// timestamps it would be an arbitrary victim, and a delivery
				// that just succeeded is the one worth keeping.
				continue
			}
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = k, at
			}
		}
		if oldestID == "" {
			return
		}
		delete(d.entries, oldestID)
	}
}

// size reports how many entries are stored, for tests.
func (d *dedup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}
