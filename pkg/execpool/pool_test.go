package execpool

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
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

	c.True(p.Parked("exec-1"), "a failed health check must park the executor")
	_, err := p.ClientFor("exec-1")
	c.Error(err, "a parked executor must not hand out a client")
}

// An executor restart installs a NEW liveConn under the same ID, and the old
// connection's health loop is still running — it does not learn its socket is
// dead until its next 30s tick, which is comfortably after the reconnect. Both
// the install and the delete were keyed by ID alone, so the stale loop deleted
// its own replacement, parked a healthy executor, and (once the park expired)
// fired onLost against children that were running fine.
func TestStaleHealthLoopCannotEvictItsReplacement(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	lc1 := &liveConn{done: make(chan struct{})}
	lc2 := &liveConn{done: make(chan struct{})}

	p.installLive("exec-1", lc1)
	p.installLive("exec-1", lc2) // executor restarts

	// The OLD loop now notices its dead socket.
	p.onHealthFailure("exec-1", lc1)

	c.False(p.Parked("exec-1"), "a stale health loop parked a live executor")
	p.mu.RLock()
	got := p.live["exec-1"]
	p.mu.RUnlock()
	c.Eq(lc2, got, "the stale loop evicted its own replacement")
	_, err := p.ClientFor("exec-1")
	c.NoError(err, "the replacement must still serve clients")
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
	assert.NewAborting(t).False(!ok || got != lc2, "a departing connection removed the entry belonging to its replacement")
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
	assert.NewAborting(t).False(p.Parked("exec-1"), "parked an executor that is live")
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
	_, err := p.ClientFor("napping")
	assert.NewCollecting(t).ErrorIs(err, ErrParked, "a parked executor must report ErrParked so the caller knows to WAIT, got")
}

// The park timeout is what converts "may return" into "gone". Until it fires
// the children wait; after it fires they must be TOLD.
func TestExpiredParkNotifiesRatherThanOnlyLogging(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.False(len(lost) != 1 || lost[0] != "gone", "an expired park must notify; got %v", lost)
	c.False(p.Parked("gone"), "an expired entry must be removed")
	_, err := p.ClientFor("gone")
	c.ErrorIs(err, ErrExecutorLost, "after the timeout it is lost, not parked")
}

// Reconnecting with the same identity reattaches. This is the sleeping-laptop
// case, and conflating it with "gone" is what the three-way split prevents.
func TestReconnectBeforeTheTimeoutClearsThePark(t *testing.T) {
	p := New(nil)
	p.Park("napping", time.Minute)
	p.reattach("napping")
	assert.NewAborting(t).False(p.Parked("napping"), "a reconnect must clear the park")
}

// Draining is learned at DISPATCH, not after a polling interval. That is the
// whole reason Leave is not an executor-initiated RPC.
func TestDrainingIsLearnedOnTheNextCall(t *testing.T) {
	p := New(nil)
	lc := &liveConn{done: make(chan struct{}), draining: true}
	p.live["exec-1"] = lc
	_, err := p.ClientFor("exec-1")
	assert.NewAborting(t).ErrorIs(err, ErrDraining, "a draining executor must report ErrDraining so the caller can pick another; got")
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
	assert.NewAborting(t).Len(live, 1, "Live() = %d entries, want 1", len(live))
	if !live[0].ConnectedAt.Equal(want) {
		t.Errorf("ConnectedAt = %v, want %v", live[0].ConnectedAt, want)
	}
}

// The callback must fire with no pool lock held. A callback that blocks while
// holding Pool.mu wedges Live(), ClientFor() and every subsequent accept — one
// unwell executor taking the whole executor plane down. This has shipped once.
func TestOnConnectFiresWithoutHoldingTheLock(t *testing.T) {
	p := New(nil)
	done := make(chan struct{})
	p.SetOnConnect(func(string) {
		// If the callback ran under p.mu, this read deadlocks and the test
		// times out rather than failing cleanly — which is the signal.
		_ = p.Live()
		close(done)
	})

	p.fireOnConnect("exec-1")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onConnect deadlocked: it is being called while Pool.mu is held")
	}
}

func TestOnConnectIsOptional(t *testing.T) {
	p := New(nil)
	p.fireOnConnect("exec-1") // must not panic with no callback set
}

// DisconnectOwner is the user-removal cut: every connection the removed user
// owns — durable or transient — is torn down through Evict, the pool's own
// close path, so the ordinary bookkeeping (removal from the parked set, the
// conn close, handleConn's exit) runs. Anyone else's executor, and above all
// every unowned one, must be untouched.
func TestDisconnectOwnerCutsOwnedExecutors(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	owned := &liveConn{executor: executors.Executor{ID: "exec-owned", OwnerUserID: "u1"}, done: make(chan struct{})}
	transient := &liveConn{executor: executors.Executor{ID: "sess-transient", OwnerUserID: "u1"}, done: make(chan struct{})}
	other := &liveConn{executor: executors.Executor{ID: "exec-other", OwnerUserID: "u2"}, done: make(chan struct{})}
	unowned := &liveConn{executor: executors.Executor{ID: "exec-free"}, done: make(chan struct{})}
	p.installLive("exec-owned", owned)
	p.installLive("sess-transient", transient)
	p.installLive("exec-other", other)
	p.installLive("exec-free", unowned)

	c.Eq(2, p.DisconnectOwner("u1"), "both of u1's connections must be counted, other owners not")

	for name, lc := range map[string]*liveConn{"owned": owned, "transient": transient} {
		select {
		case <-lc.done:
		case <-time.After(2 * time.Second):
			t.Errorf("the %s connection was counted but never torn down", name)
		}
	}
	p.mu.RLock()
	_, ownedLive := p.live["exec-owned"]
	_, transientLive := p.live["sess-transient"]
	p.mu.RUnlock()
	c.False(ownedLive || transientLive, "cut connections must leave the live set, not linger half-dead")

	for name, lc := range map[string]*liveConn{"other": other, "unowned": unowned} {
		select {
		case <-lc.done:
			t.Errorf("the %s connection must survive a foreign owner's removal", name)
		default:
		}
	}
}

// An empty userID means "unowned" on the executor row, so it must cut
// nothing — a bug that routed an unresolved owner here must not mass-
// disconnect every unowned executor in the pool. A nil pool cuts nothing
// either: a Controller built without one must not panic.
func TestDisconnectOwnerEmptyIsNoop(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	unowned := &liveConn{executor: executors.Executor{ID: "exec-free"}, done: make(chan struct{})}
	owned := &liveConn{executor: executors.Executor{ID: "exec-owned", OwnerUserID: "u1"}, done: make(chan struct{})}
	p.installLive("exec-free", unowned)
	p.installLive("exec-owned", owned)

	c.Eq(0, p.DisconnectOwner(""), "an empty owner must never cut")
	select {
	case <-unowned.done:
		t.Error("DisconnectOwner(\"\") closed an unowned executor")
	default:
	}
	select {
	case <-owned.done:
		t.Error("DisconnectOwner(\"\") closed an owned executor")
	default:
	}

	var nilPool *Pool
	c.Eq(0, nilPool.DisconnectOwner("u1"), "a nil pool is a no-op, not a panic")
}
