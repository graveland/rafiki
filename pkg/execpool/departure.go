package execpool

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Departure sentinel errors. The caller checks for these with errors.Is
// after a tool call fails — they are typed so a model can reason about them
// and a coordinator can decide whether to reschedule.
var (
	// ErrDraining means the executor is shutting down gracefully. In-flight
	// work may still complete; new work is refused. Children on ephemeral
	// workspaces may be rescheduled elsewhere.
	ErrDraining = errors.New("execpool: executor is draining; choose another executor or wait for a replacement")

	// ErrExecutorLost means the executor is gone permanently — a destroyed
	// VM, a revoked row, or a park timeout that expired. Children cannot
	// be rescheduled without a workspace rebuild.
	ErrExecutorLost = errors.New("execpool: executor lost; it will not return")

	// ErrParked means the executor's connection dropped and the pool is
	// holding its children in a parked state in case it reconnects.
	ErrParked = errors.New("execpool: executor connection lost; children are parked pending reconnect")
)

// Park places executorID into a parked state after a connection drop. If the
// executor reconnects with the same credential within timeout, parked children
// reattach. After timeout, parked entries are converted to executor-lost.
func (p *Pool) Park(executorID string, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.live[executorID]; ok {
		// Already back — must have reconnected before we parked.
		return
	}

	entry := &parkedEntry{
		executorID: executorID,
		deadline:   time.Now().Add(timeout),
	}

	// Remove previous park entry if re-parking.
	p.parked[executorID] = entry
	slog.Info("execpool: executor parked", "executorId", executorID, "timeout", timeout)
}

// Reattach is called when an executor reconnects after being parked. It
// removes the park entry so children resume normal operation.
func (p *Pool) reattach(executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.parked, executorID)
	slog.Info("execpool: executor reattached after park", "executorId", executorID)
}

// Parked returns true if executorID is currently parked.
func (p *Pool) Parked(executorID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.parked[executorID]
	return ok
}

// parkSweep runs periodically, converting expired park entries to
// executor-lost. Called from the Pool's background goroutine.
func (p *Pool) parkSweep(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweepParkedOnce(time.Now())
		}
	}
}

// SetOnLost registers a callback fired when a parked executor's timeout expires
// and it is declared permanently lost. Without it an expired park only reaches
// the log, so nothing upstream ever learns the executor is gone for good.
//
// Invoked OUTSIDE the pool lock, so a callback may safely call back into the
// pool -- the same discipline routing.Breaker.SetOnTransition documents, and
// for the same reason: p.mu is not reentrant.
func (p *Pool) SetOnLost(fn func(executorID string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onLost = fn
}

// sweepParkedOnce converts every park entry whose deadline has passed into
// executor-lost. Split out of parkSweep's ticker loop so the conversion is
// testable without waiting on a 30s tick.
//
// The callbacks fire after the lock is dropped: onLost is caller-supplied and
// may re-enter the pool, and holding p.mu across it is the same deadlock class
// as onHealthFailure's.
func (p *Pool) sweepParkedOnce(now time.Time) {
	p.mu.Lock()
	var lost []string
	for id, entry := range p.parked {
		if now.After(entry.deadline) {
			delete(p.parked, id)
			lost = append(lost, id)
		}
	}
	for id, t := range p.evicted {
		if now.Sub(t) > parkTimeout {
			delete(p.evicted, id)
		}
	}
	fn := p.onLost
	p.mu.Unlock()

	for _, id := range lost {
		slog.Warn("execpool: parked executor lost (timeout)", "executorId", id)
		if fn != nil {
			fn(id)
		}
	}
}

type parkedEntry struct {
	executorID string
	deadline   time.Time
}

// ClientFor now also checks parked state and returns typed errors.
// Override the previous ClientFor by wrapping it.
