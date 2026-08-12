package execpool

import (
	"errors"
	"sync"
	"testing"
	"time"
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
