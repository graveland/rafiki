package execpool

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
)

// The bug, reproduced: healthLoop holds p.mu and calls Park, which takes p.mu
// again. sync.RWMutex is not reentrant, so the goroutine blocks forever WHILE
// HOLDING the pool lock — every later Live(), ClientFor() and accept blocks
// with it. One unwell executor takes the whole daemon's executor plane down.
//
// Driven through a timeout rather than by calling healthLoop directly, because
// a deadlocked test does not fail, it hangs — and a hang in CI reads as
// infrastructure trouble rather than as this.
func TestHealthFailureParksWithoutWedgingThePool(t *testing.T) {
	p := New(nil)
	lc := &liveConn{done: make(chan struct{})}
	p.live["exec-1"] = lc

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.onHealthFailure("exec-1", lc)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onHealthFailure did not return: the pool lock is held by a goroutine waiting for the pool lock")
	}

	// And the pool must still be usable afterwards.
	acquired := make(chan struct{})
	go func() {
		_ = p.Live()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Live() blocked after a health failure — the lock was never released")
	}

	if !p.Parked("exec-1") {
		t.Fatal("a failed health check must park the executor")
	}
	if _, err := p.ClientFor("exec-1"); err == nil {
		t.Fatal("a parked executor must not hand out a client")
	}
}

// An executor restart installs a NEW liveConn under the same ID, and the old
// connection's health loop is still running — it does not learn its socket is
// dead until its next 30s tick, which is comfortably after the reconnect. Both
// the install and the delete were keyed by ID alone, so the stale loop deleted
// its own replacement, parked a healthy executor, and (once the park expired)
// fired onLost against children that were running fine.
func TestStaleHealthLoopCannotEvictItsReplacement(t *testing.T) {
	p := New(nil)
	lc1 := &liveConn{done: make(chan struct{})}
	lc2 := &liveConn{done: make(chan struct{})}

	p.installLive("exec-1", lc1)
	p.installLive("exec-1", lc2) // executor restarts

	// The OLD loop now notices its dead socket.
	p.onHealthFailure("exec-1", lc1)

	if p.Parked("exec-1") {
		t.Fatal("a stale health loop parked a live executor")
	}
	p.mu.RLock()
	got := p.live["exec-1"]
	p.mu.RUnlock()
	if got != lc2 {
		t.Fatal("the stale loop evicted its own replacement")
	}
	if _, err := p.ClientFor("exec-1"); err != nil {
		t.Fatalf("the replacement must still serve clients: %v", err)
	}
}

// The same identity check on handleConn's exit path. This one is reached by
// every ordinary disconnect, so without it a slow-closing old connection
// evicts the replacement it was displaced by.
func TestHandleConnExitDoesNotEvictAReplacement(t *testing.T) {
	p := New(nil)
	lc1 := &liveConn{done: make(chan struct{})}
	lc2 := &liveConn{done: make(chan struct{})}

	p.installLive("exec-1", lc1)
	p.installLive("exec-1", lc2)

	p.removeLive("exec-1", lc1) // lc1's handleConn returning, late

	p.mu.RLock()
	got, ok := p.live["exec-1"]
	p.mu.RUnlock()
	if !ok || got != lc2 {
		t.Fatal("a departing connection removed the entry belonging to its replacement")
	}
}

// Displacing a connection must TEAR IT DOWN. Its handleConn is parked on
// <-lc.done and its healthLoop on the same channel; if nothing closes it they
// both live forever, holding a TLS connection and writing TouchSeen every 30s
// for an executor that left. Leaking one of these per reconnect is a slow
// resource leak that looks like nothing until a laptop has slept a hundred
// times.
func TestDisplacedConnectionIsTornDown(t *testing.T) {
	p := New(nil)
	lc1 := &liveConn{done: make(chan struct{})}
	lc2 := &liveConn{done: make(chan struct{})}

	p.installLive("exec-1", lc1)
	p.installLive("exec-1", lc2)

	select {
	case <-lc1.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the displaced connection was never signalled; its handleConn and healthLoop leak")
	}
	select {
	case <-lc2.done:
		t.Fatal("the replacement was torn down instead of the connection it displaced")
	default:
	}
}

// Teardown arrives from two directions — the displacing install and the
// connection's own health failure — and closing a channel twice is a panic
// that takes the daemon with it, not a recoverable error.
func TestTeardownIsIdempotent(t *testing.T) {
	p := New(nil)
	lc := &liveConn{done: make(chan struct{})}
	p.installLive("exec-1", lc)
	p.installLive("exec-1", &liveConn{done: make(chan struct{})}) // displaces lc

	// lc's own health loop now fails, after it was already torn down.
	p.onHealthFailure("exec-1", lc)
	lc.shutdown()
}

// Re-parking an executor that reconnected in the meantime must not evict the
// live connection. The existing Park has this check; moving the lock must not
// lose it.
func TestParkIsANoopWhenTheExecutorIsAlreadyBack(t *testing.T) {
	p := New(nil)
	p.live["exec-1"] = &liveConn{done: make(chan struct{})}
	p.Park("exec-1", time.Minute)
	if p.Parked("exec-1") {
		t.Fatal("parked an executor that is live")
	}
}

// The three errors exist to be RETURNED. Today nothing returns any of them:
// grep -rn "ErrExecutorLost|ErrDraining|ErrParked" pkg cmd finds only the
// declarations. A typed error a model can reason about is not a typed error
// until something produces it.
func TestClientForReturnsTypedDepartureErrors(t *testing.T) {
	p := New(nil)

	if _, err := p.ClientFor("never-seen"); !errors.Is(err, ErrExecutorLost) {
		t.Errorf("an executor that was never here is lost, got %v", err)
	}

	p.Park("napping", time.Minute)
	if _, err := p.ClientFor("napping"); !errors.Is(err, ErrParked) {
		t.Errorf("a parked executor must report ErrParked so the caller knows to WAIT, got %v", err)
	}
}

// The park timeout is what converts "may return" into "gone". Until it fires
// the children wait; after it fires they must be TOLD.
func TestExpiredParkNotifiesRatherThanOnlyLogging(t *testing.T) {
	p := New(nil)
	var lost []string
	var mu sync.Mutex
	p.SetOnLost(func(id string) {
		mu.Lock()
		defer mu.Unlock()
		lost = append(lost, id)
	})
	p.Park("gone", -time.Second) // already expired
	p.sweepParkedOnce(time.Now())

	mu.Lock()
	defer mu.Unlock()
	if len(lost) != 1 || lost[0] != "gone" {
		t.Fatalf("an expired park must notify; got %v", lost)
	}
	if p.Parked("gone") {
		t.Fatal("an expired entry must be removed")
	}
	if _, err := p.ClientFor("gone"); !errors.Is(err, ErrExecutorLost) {
		t.Fatalf("after the timeout it is lost, not parked: %v", err)
	}
}

// Reconnecting with the same identity reattaches. This is the sleeping-laptop
// case, and conflating it with "gone" is what the three-way split prevents.
func TestReconnectBeforeTheTimeoutClearsThePark(t *testing.T) {
	p := New(nil)
	p.Park("napping", time.Minute)
	p.reattach("napping")
	if p.Parked("napping") {
		t.Fatal("a reconnect must clear the park")
	}
}

// Draining is learned at DISPATCH, not after a polling interval. That is the
// whole reason Leave is not an executor-initiated RPC.
func TestDrainingIsLearnedOnTheNextCall(t *testing.T) {
	p := New(nil)
	lc := &liveConn{done: make(chan struct{}), draining: true}
	p.live["exec-1"] = lc
	if _, err := p.ClientFor("exec-1"); !errors.Is(err, ErrDraining) {
		t.Fatalf("a draining executor must report ErrDraining so the caller can pick another; got %v", err)
	}
}

// Live() must report when a connection was established, not just that it
// currently is one — a client watching `rafiki executor list` wants to know
// how long a connection has held, not merely that it's up right now.
func TestLiveReportsConnectedAt(t *testing.T) {
	p := New(nil)
	want := time.Now().Add(-5 * time.Minute)
	lc := &liveConn{
		done:        make(chan struct{}),
		executor:    executors.Executor{ID: "exec-1"},
		describe:    &executorpb.DescribeResponse{},
		connectedAt: want,
	}
	p.live["exec-1"] = lc

	live := p.Live()
	if len(live) != 1 {
		t.Fatalf("Live() = %d entries, want 1", len(live))
	}
	if !live[0].ConnectedAt.Equal(want) {
		t.Errorf("ConnectedAt = %v, want %v", live[0].ConnectedAt, want)
	}
}
