package bus

import (
	"sync"
	"sync/atomic"
)

// Options configures a Bus.
type Options struct {
	// PerSubBuffer is the channel buffer depth per subscriber. Defaults to 256.
	PerSubBuffer int
}

// Stats holds a point-in-time snapshot of bus health.
type Stats struct {
	SubscriberCount int
	TotalDrops      uint64
}

// Bus is a generic single-producer fan-out with bounded per-subscriber channels.
// When a subscriber's channel is full, Publish drops the event for that subscriber
// and increments the drop counter rather than blocking.
type Bus[T any] struct {
	opts       Options
	mu         sync.Mutex
	subs       map[uint64]*subscriber[T]
	nextID     uint64
	totalDrops atomic.Uint64
	closed     atomic.Bool
}

// subscriber holds a single subscription's state.
//
// Race-safety design:
//   - mu serialises the send in Publish with the close in cancel/Close.
//   - closed is set to true under mu before ch is closed, so Publish's guard
//     (if !s.closed) ensures ch is never sent to after it has been closed.
//
// Note: a 3-way select { case <-done: | case ch<-v: | default } does NOT
// prevent "send on closed channel" because Go's select evaluates cases in
// random order and panics when it reaches a send on a closed channel, even
// if another ready case (e.g. <-done) exists. The closed flag + mutex is the
// correct guard.
type subscriber[T any] struct {
	id     uint64
	ch     chan T
	mu     sync.Mutex // serialises ch send (Publish) with ch close (cancel/Close)
	closed bool       // set to true before ch is closed, always under mu
}

// New creates a Bus with the given options.
func New[T any](opts Options) *Bus[T] {
	if opts.PerSubBuffer <= 0 {
		opts.PerSubBuffer = 256
	}
	return &Bus[T]{
		opts: opts,
		subs: make(map[uint64]*subscriber[T]),
	}
}

// Subscribe registers a new subscriber and returns its receive channel and a
// cancel function. Calling cancel removes the subscriber and closes the channel.
// If the Bus is already closed, Subscribe returns an already-closed channel and
// a no-op cancel.
func (b *Bus[T]) Subscribe() (<-chan T, func()) {
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		ch := make(chan T)
		close(ch)
		return ch, func() {}
	}
	b.nextID++
	s := &subscriber[T]{
		id: b.nextID,
		ch: make(chan T, b.opts.PerSubBuffer),
	}
	b.subs[s.id] = s
	b.mu.Unlock()

	cancel := func() {
		// Remove from the live map first so no new Publish snapshot includes s.
		b.mu.Lock()
		_, ok := b.subs[s.id]
		if ok {
			delete(b.subs, s.id)
		}
		b.mu.Unlock()

		if !ok {
			return // idempotent: already cancelled or bus was closed first
		}

		// Set closed and close ch under s.mu so that Publish's guard
		// (if !s.closed) and close(s.ch) are never concurrent.
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	}
	return s.ch, cancel
}

// Publish sends v to every subscriber. If a subscriber's buffer is full, the
// event is dropped for that subscriber and the drop counter is incremented.
// Publish never blocks.
func (b *Bus[T]) Publish(v T) {
	// Fast path: skip dispatching when bus is closed. This is a throughput
	// shortcut, not a correctness guard — the per-subscriber closed flag
	// + mutex in the dispatch loop is what makes concurrent Close safe.
	if b.closed.Load() {
		return
	}
	b.mu.Lock()
	subs := make([]*subscriber[T], 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		// Hold s.mu for the check-and-send so that close(s.ch) in cancel/Close
		// cannot execute concurrently with the send. s.closed is set to true
		// before close(s.ch), so this guard is sufficient.
		s.mu.Lock()
		if !s.closed {
			select {
			case s.ch <- v:
			default:
				b.totalDrops.Add(1)
			}
		}
		s.mu.Unlock()
	}
}

// Stats returns a point-in-time snapshot of subscriber count and total drops.
func (b *Bus[T]) Stats() Stats {
	b.mu.Lock()
	n := len(b.subs)
	b.mu.Unlock()
	return Stats{
		SubscriberCount: n,
		TotalDrops:      b.totalDrops.Load(),
	}
}

// Close shuts down the bus, closing all subscriber channels. Idempotent.
func (b *Bus[T]) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	// Take ownership of the subs map and release b.mu before per-subscriber
	// work to keep b.mu hold time short and avoid nested lock acquisition.
	b.mu.Lock()
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for _, s := range subs {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	}
}
