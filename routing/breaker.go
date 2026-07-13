package routing

import (
	"sync"
	"time"
)

// Breaker is a shared Anthropic-health circuit breaker. Closed → use the
// primary. A retryable failure trips it Open, pinning callers to the fallback
// until probeInterval elapses; then exactly one caller is allowed a primary
// probe — a healthy probe closes it, a failed probe re-pins for another
// probeInterval.
type Breaker struct {
	mu            sync.Mutex
	probeInterval time.Duration
	open          bool
	lastProbe     time.Time // last time a probe was allowed (or the trip time)
	onTransition  func(open bool, now time.Time)
}

func NewBreaker(probeInterval time.Duration) *Breaker {
	return &Breaker{probeInterval: probeInterval}
}

// SetOnTransition registers a callback fired when the open state CHANGES
// (trip or close — not probe-slot consumption). Invoked outside the breaker's
// lock. Consumed by metrics/logging: a silent 15-minute pin of every consumer
// would otherwise be invisible.
func (b *Breaker) SetOnTransition(fn func(open bool, now time.Time)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onTransition = fn
}

// Open reports the current state (true = pinned to fallback), for gauges.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// UsePrimary reports whether this call should hit the primary. When open it
// allows a single probe once probeInterval has elapsed since the last trip/probe.
func (b *Breaker) UsePrimary(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if now.Sub(b.lastProbe) >= b.probeInterval {
		b.lastProbe = now // consume the probe slot
		return true
	}
	return false
}

// RecordResult updates state from a primary attempt. retryableFailure==true
// (5xx/429/timeout) trips/keeps Open and stamps the window; success closes.
func (b *Breaker) RecordResult(now time.Time, retryableFailure bool) {
	b.mu.Lock()
	was := b.open
	if retryableFailure {
		b.open = true
		b.lastProbe = now
	} else {
		b.open = false
	}
	changed := b.open != was
	open := b.open
	fn := b.onTransition
	b.mu.Unlock()
	// Invoked OUTSIDE the lock so a callback may safely call back into the
	// breaker (Open(), etc.) without deadlocking.
	if changed && fn != nil {
		fn(open, now)
	}
}
