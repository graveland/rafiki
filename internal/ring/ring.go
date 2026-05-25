package ring

import "sync"

// Event is a single stored record: raw bytes plus the publish timestamp (unix ms).
type Event struct {
	Bytes     []byte
	Timestamp int64 // unix ms
}

// Options configures the ring's eviction limits.
type Options struct {
	MaxEvents int // max event count; default 5000
	MaxBytes  int // max total byte size; default 64 MiB
}

// Query describes which events Recent should return.
type Query struct {
	Limit int   // 0 means no limit
	Since int64 // 0 means no time filter; otherwise return events with Timestamp >= Since (inclusive)
}

// Ring is a bounded LRU event buffer. Oldest events are dropped when either
// the event count or the total byte size exceeds the configured limits.
// It is safe for concurrent use.
type Ring struct {
	opts       Options
	mu         sync.Mutex
	events     []Event
	totalBytes int
}

// New creates a Ring with the given options. Zero/negative option fields use
// defaults: 5000 events and 64 MiB.
func New(opts Options) *Ring {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 5000
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 64 << 20
	}
	return &Ring{opts: opts}
}

// Append adds an event to the ring. The provided bytes are copied so the
// caller may reuse the slice immediately. Eviction runs after each append.
func (r *Ring) Append(payload []byte, ts int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the bytes — caller may reuse the slice.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	r.events = append(r.events, Event{Bytes: cp, Timestamp: ts})
	r.totalBytes += len(cp)
	r.evict()
}

// evict drops the oldest events until both caps are satisfied.
// Must be called with r.mu held.
func (r *Ring) evict() {
	for len(r.events) > r.opts.MaxEvents {
		r.totalBytes -= len(r.events[0].Bytes)
		r.events = r.events[1:]
	}
	for r.totalBytes > r.opts.MaxBytes && len(r.events) > 0 {
		r.totalBytes -= len(r.events[0].Bytes)
		r.events = r.events[1:]
	}
	// Avoid unbounded backing-array growth from repeated front-slicing.
	if cap(r.events) > 2*r.opts.MaxEvents && len(r.events) < r.opts.MaxEvents/2 {
		r.events = append([]Event(nil), r.events...)
	}
}

// Recent returns a snapshot of stored events matching the query. The returned
// slice and its Bytes fields are defensive copies; callers may not mutate ring
// storage through them.
func (r *Ring) Recent(q Query) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	src := r.events
	if q.Since > 0 {
		i := 0
		for i < len(src) && src[i].Timestamp < q.Since {
			i++
		}
		src = src[i:]
	}
	if q.Limit > 0 && len(src) > q.Limit {
		src = src[len(src)-q.Limit:]
	}
	out := make([]Event, len(src))
	for i, ev := range src {
		// Defensive copy so callers can't mutate the ring's storage.
		cp := make([]byte, len(ev.Bytes))
		copy(cp, ev.Bytes)
		out[i] = Event{Bytes: cp, Timestamp: ev.Timestamp}
	}
	return out
}

// Stats returns the current event count, total byte size, and the timestamp of
// the oldest stored event. Returns zero values when the ring is empty.
func (r *Ring) Stats() (events, bytes int, oldestTimestamp int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return 0, 0, 0
	}
	return len(r.events), r.totalBytes, r.events[0].Timestamp
}
