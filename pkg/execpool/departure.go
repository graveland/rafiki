package execpool

import (
	"log/slog"
	"time"
)

// Park places executorID into a parked state after a connection drop. If the
// executor reconnects with the same credential within timeout, parked children
// reattach; after timeout the entry becomes executor-lost.
func (p *Pool) Park(executorID string, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parkLocked(executorID, timeout)
}

// parkLocked is Park's body for callers that already hold p.mu.
//
// The split exists because sync.RWMutex is not reentrant and the pool's mutex
// guards the accept path: a goroutine that blocks while holding it does not
// just lose its own executor, it wedges Live(), ClientFor() and every
// subsequent accept. Any new caller inside a locked region must use THIS, and
// any new caller outside one must use Park — never reach for the other.
func (p *Pool) parkLocked(executorID string, timeout time.Duration) {
	if _, ok := p.live[executorID]; ok {
		return // already back; it reconnected before we got here
	}
	p.parked[executorID] = &parkedEntry{
		executorID: executorID,
		deadline:   time.Now().Add(timeout),
	}
	slog.Info("execpool: executor parked", "executorId", executorID, "timeout", timeout)
}

// parkSweep runs periodically to convert expired parks into lost notifications.
//
//nolint:unused // wired by full daemon integration
func (p *Pool) parkSweep() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.sweepParkedOnce(time.Now())
	}
}

// sweepParkedOnce converts every park whose deadline has passed into
// executor-lost, and notifies. Split out of parkSweep so a test can drive one
// tick without waiting 30 seconds or reaching into the ticker.
func (p *Pool) sweepParkedOnce(now time.Time) {
	p.mu.Lock()
	var expired []string
	for id, entry := range p.parked {
		if now.After(entry.deadline) {
			delete(p.parked, id)
			expired = append(expired, id)
		}
	}
	onLost := p.onLost
	p.mu.Unlock()

	// Notify OUTSIDE the lock. The callback ends at Controller.Send — a
	// blocking write to a child — and a producer that touches the pool from
	// inside it would deadlock on a re-entrant acquire.
	for _, id := range expired {
		slog.Warn("execpool: parked executor lost (timeout)", "executorId", id)
		if onLost != nil {
			onLost(id)
		}
	}
}
