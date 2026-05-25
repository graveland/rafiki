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

type subscriber[T any] struct {
	id    uint64
	ch    chan T
	drops atomic.Uint64
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
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s.id]; ok {
			delete(b.subs, s.id)
			close(s.ch)
		}
	}
	return s.ch, cancel
}

// Publish sends v to every subscriber. If a subscriber's buffer is full, the
// event is dropped for that subscriber and the drop counters are incremented.
// Publish never blocks.
func (b *Bus[T]) Publish(v T) {
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
		select {
		case s.ch <- v:
		default:
			s.drops.Add(1)
			b.totalDrops.Add(1)
		}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		close(s.ch)
	}
	b.subs = nil
}
