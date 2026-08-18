package execpool

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// A relabel must reach a LIVE connection.
//
// `conversations.executors` is authoritative and the design leans on that: an
// operator relabels a machine with `rafiki executor label`, a row update needing
// no reissue, no restart and no access to the machine. But the row was read
// exactly once, at Authenticate during the handshake, and cached in
// liveConn.executor for the connection's lifetime — and selection reads that
// cache through Live(). Executor connections are deliberately long-lived, so in
// practice the labels were frozen at enrollment.
func TestRelabellingReachesALiveConnection(t *testing.T) {
	store := newFakeStore("exec-1")
	store.update(func(e *executors.Executor) { e.Labels = map[string]string{"env": "home"} })

	p := New(store)
	p.healthInterval = 20 * time.Millisecond
	p.healthTimeout = 500 * time.Millisecond

	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-1"}))

	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })
	if got := p.Live()[0].Executor.Labels["env"]; got != "home" {
		t.Fatalf("joined with env=%q, want home", got)
	}

	// The operator relabels. The executor process is untouched and still
	// connected.
	store.update(func(e *executors.Executor) { e.Labels = map[string]string{"env": "work"} })

	waitFor(t, 5*time.Second, "the new label to reach the live pool", func() bool {
		live := p.Live()
		return len(live) == 1 && live[0].Executor.Labels["env"] == "work"
	})
}

// Revocation must reach a live connection too, and this is the half that
// matters: `rafiki executor disable` is how an operator cuts off a machine they
// may no longer trust. Taking effect only at the next reconnect means a
// long-lived connection keeps serving indefinitely — which for a laptop that
// stays connected for days is indistinguishable from not working at all.
func TestDisablingAnExecutorRemovesItFromTheLivePool(t *testing.T) {
	store := newFakeStore("exec-2")
	p := New(store)
	p.healthInterval = 20 * time.Millisecond
	p.healthTimeout = 500 * time.Millisecond

	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-2"}))

	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	store.update(func(e *executors.Executor) { e.Enabled = false })

	waitFor(t, 5*time.Second, "the disabled executor to leave the live pool", func() bool {
		return len(p.Live()) == 0
	})
}

// A row that cannot be READ is not a revocation.
//
// This is the A3 lesson in a second place: "this credential is not valid" is an
// answer, "I could not check" is not, and treating the second as the first turns
// a database blip into a fleet-wide outage — every executor's health loop
// evicting itself at the same moment. An unreadable row keeps the last known one
// and tries again on the next tick.
func TestAnUnreadableRowDoesNotEvictAHealthyExecutor(t *testing.T) {
	store := newFakeStore("exec-3")
	p := New(store)
	p.healthInterval = 20 * time.Millisecond
	p.healthTimeout = 500 * time.Millisecond

	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-3"}))
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	// Get now fails for this id — the fake returns an error for any id it does
	// not recognise, so renaming the row makes every lookup fail the way an
	// unreachable database would.
	store.update(func(e *executors.Executor) { e.ID = "someone-else" })

	// Give the health loop several ticks to do the wrong thing.
	time.Sleep(200 * time.Millisecond)

	if len(p.Live()) != 1 {
		t.Fatal("a healthy executor was evicted because its row could not be read; " +
			"'could not check' is not 'revoked'")
	}
}

// One credential names one row, and the pool holds one connection per row. A
// second connection while the first is ALIVE is either a credential on two
// machines or an attacker with a stolen one; either way the incumbent — which
// is answering — is the one to keep.
//
// This used to displace instead, so a stolen credential could kick the real
// executor off its own daemon, and the two would flap indefinitely with every
// displacement looking like an ordinary reconnect in the log.
func TestASecondConnectionIsRefusedWhileTheFirstIsAlive(t *testing.T) {
	store := newFakeStore("exec-dup")
	p := New(store)
	p.healthInterval = time.Hour // no health loop interference
	p.healthTimeout = 2 * time.Second

	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-dup"}))
	waitFor(t, 5*time.Second, "the first executor to join", func() bool { return len(p.Live()) == 1 })
	first := p.Live()[0]

	// A second, healthy connection presenting the same credential.
	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-dup"}))

	// Give it long enough to have displaced the incumbent if it were going to.
	time.Sleep(500 * time.Millisecond)

	live := p.Live()
	if len(live) != 1 {
		t.Fatalf("expected exactly one live connection, got %d", len(live))
	}
	if live[0].Describe != first.Describe {
		t.Fatal("the second connection displaced a live incumbent; a stolen credential " +
			"must not be able to evict the executor it was stolen from")
	}
}

// The other half, and the reason this is a liveness probe rather than
// first-come-wins: a laptop that sleeps or loses its network frequently leaves
// NO FIN or RST behind, so the daemon still holds a connection that is dead.
// Refusing the real executor until a health tick noticed would strand it.
func TestAConnectionThatNoLongerAnswersIsReplaced(t *testing.T) {
	store := newFakeStore("exec-stale")
	p := New(store)
	p.healthInterval = time.Hour
	p.healthTimeout = 300 * time.Millisecond

	// The incumbent accepts and then answers nothing — a black hole, which is
	// what a slept laptop looks like from here.
	go p.handleConn(invertedPair(t, &blackHoleHandler{executorID: "exec-stale"}))
	waitFor(t, 5*time.Second, "the black hole to join", func() bool { return len(p.Live()) == 1 })

	// The real executor comes back.
	go p.handleConn(invertedPair(t, &stubHandler{executorID: "exec-stale"}))

	waitFor(t, 10*time.Second, "the answering connection to take over", func() bool {
		live := p.Live()
		return len(live) == 1 && len(live[0].Describe.Tools) > 0
	})
}
