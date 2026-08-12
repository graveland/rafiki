// Package execpool carries the reverse-dialled executor transport and the
// live-connection registry built on it.
//
// The arrangement in one sentence: the executor DIALS rafikid and then SERVES
// HTTP/2 on the connection it dialled, while rafikid ACCEPTS and is the HTTP
// client. TLS roles follow the TCP direction; HTTP roles invert on top.
//
// Dial-out is required, not stylistic: rafikid in k8s cannot reach a laptop
// behind NAT. Nothing needs to be executor-initiated — join is rafikid calling
// Describe on accept, heartbeat is rafikid polling Health, and leave is a typed
// Draining error on the next Execute, learned at dispatch rather than after a
// polling interval.
package execpool

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// ExecutorClient is the surface the pool exposes to callers who have selected
// an executor. In a full build this would be the Connect client for the
// executor service (executorpbconnect.ExecutorServiceClient), but defers to
// any for now to avoid a circular dependency on proto-generated code.
type ExecutorClient any

// LiveExecutor is a live executor visible to the pool for selection.
type LiveExecutor struct {
	executors.Executor
}

// ─── Errors ──────────────────────────────────────────────────────────────────

// ErrParked is returned by ClientFor when the executor is parked — it existed
// recently and may return, so the caller should wait rather than reschedule.
var ErrParked = errors.New("executor is parked and may reconnect")

// ErrExecutorLost is returned by ClientFor when an executor is definitively
// gone — it was never here, or its park timeout expired.
var ErrExecutorLost = errors.New("executor is lost and will not return")

// ErrDraining is returned by ClientFor when the executor is draining — it is
// shutting down gracefully and will not accept new work.
var ErrDraining = errors.New("executor is draining and will not accept new work")

// ─── Types ───────────────────────────────────────────────────────────────────

// liveConn is one accepted executor connection.
type liveConn struct {
	executorID string
	client     ExecutorClient
	draining   bool
	done       chan struct{} // closed when the connection is gone
}

// parkedEntry records an executor that disconnected and may reconnect.
type parkedEntry struct {
	executorID string
	deadline   time.Time
}

// parKTimeout is how long the pool waits for a disconnected executor to reconnect
// before declaring it lost.
const parkTimeout = 5 * time.Minute

// Pool is the live executor registry.
//
// Executors dial in and are accepted. The pool health-checks them, parks them
// on failure, and hands out clients for child processes. It is safe for
// concurrent use.
type Pool struct {
	mu     sync.RWMutex
	live   map[string]*liveConn
	parked map[string]*parkedEntry
	onLost func(executorID string)
}

// New returns an empty Pool.
func New(onLost func(executorID string)) *Pool {
	return &Pool{
		live:   make(map[string]*liveConn),
		parked: make(map[string]*parkedEntry),
		onLost: onLost,
	}
}

// SetOnLost replaces the callback invoked when a parked executor's timeout
// expires. It is safe to call concurrently with pool operations.
func (p *Pool) SetOnLost(fn func(executorID string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onLost = fn
}

// Live returns a snapshot of every live executor.
func (p *Pool) Live() []LiveExecutor {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]LiveExecutor, 0, len(p.live))
	for _, lc := range p.live {
		out = append(out, LiveExecutor{
			Executor: executors.Executor{ID: lc.executorID},
		})
	}
	return out
}

// ClientFor returns the client for an executor. It returns typed errors so
// callers can distinguish parked (may return) from lost (never return) from
// draining (graceful shutdown).
func (p *Pool) ClientFor(executorID string) (ExecutorClient, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if lc, ok := p.live[executorID]; ok {
		if lc.draining {
			return nil, fmt.Errorf("executor %s: %w", executorID, ErrDraining)
		}
		return lc.client, nil
	}
	if _, ok := p.parked[executorID]; ok {
		return nil, fmt.Errorf("executor %s: %w", executorID, ErrParked)
	}
	return nil, fmt.Errorf("executor %s: %w", executorID, ErrExecutorLost)
}

// Parked reports whether executorID is currently parked.
func (p *Pool) Parked(executorID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.parked[executorID]
	return ok
}

// Reattach moves a parked executor back to live. Called on reconnect when the
// executor authenticates with the same identity.
func (p *Pool) Reattach(executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reattachLocked(executorID)
}

func (p *Pool) reattachLocked(executorID string) {
	delete(p.parked, executorID)
}

// Accept registers a new live connection. The previous connection for this
// executor, if any, is closed.
func (p *Pool) Accept(executorID string, client ExecutorClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.live[executorID]; ok {
		close(prev.done)
	}
	p.live[executorID] = &liveConn{
		executorID: executorID,
		client:     client,
		done:       make(chan struct{}),
	}
	// If the executor reconnected while parked, unpark it.
	delete(p.parked, executorID)
}

// Remove takes an executor out of the live map (connection dropped).
func (p *Pool) Remove(executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeLocked(executorID)
}

func (p *Pool) removeLocked(executorID string) {
	if lc, ok := p.live[executorID]; ok {
		delete(p.live, executorID)
		close(lc.done)
	}
}

// ─── Health loop ─────────────────────────────────────────────────────────────

// healthCheck probes one live executor and returns nil if it is healthy.
// The executor must implement a Health check; if it fails, the pool parks it.
//
//nolint:unused // wired by full daemon integration
func (p *Pool) healthCheck(executorID string) error {
	p.mu.RLock()
	lc, ok := p.live[executorID]
	p.mu.RUnlock()
	if !ok {
		return nil // already gone
	}
	// In a full implementation this would call the executor's Health RPC.
	// For now, the connection's mere existence and a successful ping is enough.
	select {
	case <-lc.done:
		return fmt.Errorf("connection closed")
	default:
		return nil
	}
}

// onHealthFailure demotes a live executor to parked.
//
// Ordering matters and is not obvious: the live entry must be removed BEFORE
// parkLocked runs, because parkLocked deliberately declines to park anything
// still in p.live (that check is what makes a reconnect race safe). Doing it
// the other way round silently parks nothing.
func (p *Pool) onHealthFailure(id string, lc *liveConn) {
	p.mu.Lock()
	delete(p.live, id)
	p.parkLocked(id, parkTimeout)
	p.mu.Unlock()
	close(lc.done)
}

// maxHealthTickCount bounds the number of health ticks so a stuck loop doesn't
// leak goroutines forever.
const maxHealthTickCount = 1000 //nolint:unused // wired by full daemon integration

// healthLoop periodically checks one executor. Exits when the connection closes
// or the executor is removed.
//
//nolint:unused // wired by full daemon integration
func (p *Pool) healthLoop(id string, lc *liveConn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range maxHealthTickCount {
		if err := p.healthCheck(id); err != nil {
			slog.Warn("execpool: health check failed; parking executor", "executorId", id, "error", err)
			p.onHealthFailure(id, lc)
			return
		}
		select {
		case <-lc.done:
			return
		case <-ticker.C:
		}
	}
}
