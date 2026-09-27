package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

func liveExecutor(id string, labels map[string]string) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{ID: id, Labels: labels, Enabled: true},
	}
}

func newSessionTestController(t *testing.T, live ...execpool.LiveExecutor) *Controller {
	t.Helper()
	return &Controller{execPool: &fakePool{live: live}}
}

func TestExecutorSessionDefersToADurableExecutorOnThisMachine(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSessionTestController(t, liveExecutor("durable-1", map[string]string{
		"owner":   "brent",
		"machine": "m-abc",
	}))

	got, err := c.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc", Roots: []string{"/src"}})
	ck.NoError(err)
	if got.RunLocal {
		t.Fatal("a durable executor already covers this machine; starting a " +
			"transient one as well offers a second executor nothing will prefer")
	}
	ck.Eq("durable-1", got.ExecutorID, "ExecutorID")
	ck.Eq("owner=brent,machine=m-abc", got.Selector, "the selector must name the machine, not the hostname")
}

func TestExecutorSessionIgnoresAnotherOwnersExecutorOnTheSameName(t *testing.T) {
	c := newSessionTestController(t, liveExecutor("sams-laptop", map[string]string{
		"owner":   "sam",
		"machine": "laptop",
	}))

	got, err := c.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"})
	assert.NewAborting(t).NoError(err)
	if !got.RunLocal {
		t.Fatal("sam's laptop is not brent's; the durable match must be scoped " +
			"to the owner or a client binds children onto another operator's box")
	}
}

func TestExecutorSessionMintsATicketWhenNoDurableExecutorExists(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSessionTestController(t)

	got, err := c.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc", Roots: []string{"/src"}})
	ck.NoError(err)
	ck.True(got.RunLocal, "with no durable executor the client must serve one itself")
	ck.NotEq("", got.Ticket, "a transient executor authenticates with a ticket")
	ck.NotEq("", got.ExecutorID, "the daemon assigns the id; the executor must never choose its own")
	ck.Eq("owner=brent,machine=m-abc", got.Selector, "%s", "selector must match the durable case so a child can move "+
		"between them without its stored selector changing; got "+got.Selector)
}

func TestExecutorSessionSelectorIsIdenticalInBothCases(t *testing.T) {
	c := assert.NewAborting(t)
	// This is what makes failover expressible INSIDE the confinement rules:
	// one stored selector names both executors, so effectiveExecutorSet can
	// hand a child the other one without the selector ever being rewritten.
	withDurable := newSessionTestController(t, liveExecutor("durable-1", map[string]string{
		"owner": "brent", "machine": "m-abc",
	}))
	without := newSessionTestController(t)

	a, err := withDurable.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc"})
	c.NoError(err)
	b, err := without.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc"})
	c.NoError(err)
	c.Eq(b.Selector, a.Selector, "selectors diverge")
}

func TestExecutorSessionRequiresAName(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSessionTestController(t)
	_, err := c.executorSession(context.Background(), nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{})
	ck.Error(err, "without a name the daemon cannot tell which durable executor "+
		"shares this client's filesystem")
	ck.StrContains(err.Error(), "rafiki executor name", "the error must tell the operator how to fix it, got: %v", err)
}

// TestASecondSessionRequestReleasesTheFirst verifies M5: a second
// executorSession under one session key releases the first. Before this
// fix, the map assignment overwrote silently: the incumbent's ticket
// was never revoked and its executor never evicted. (The framed plane's
// connection keyed these; today only a misbehaving caller could repeat a
// key, but the release-on-overwrite is the thing this pins.)
func TestASecondSessionRequestReleasesTheFirst(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := &fakePool{evicted: make(map[string]bool)}
	c := &Controller{execPool: pool}
	ctx := context.Background()
	id := users.Identity{Username: "brent"}

	first, err := c.executorSession(ctx, "one-key", id, protocol.ExecutorSessionRequest{Name: "laptop"})
	ck.NoError(err)
	second, err := c.executorSession(ctx, "one-key", id, protocol.ExecutorSessionRequest{Name: "laptop"})
	ck.NoError(err)
	ck.NotEq(second.ExecutorID, first.ExecutorID, "each request mints a distinct executor")
	ck.False(!pool.evicted[first.ExecutorID], "the incumbent %s was orphaned: Evict was never called", first.ExecutorID)

	// The ticket must be revoked too — verify through the registry.
	if _, ok := pool.Tickets().Redeem(first.Ticket); ok {
		t.Fatal("the incumbent's ticket was not revoked; it stays valid for " +
			"the daemon's lifetime")
	}
}

// syncEvictPool is a race-safe executorPool fake. fakePool's evicted map
// (executor_select_test.go) is unsynchronized, which is fine for its
// synchronous callers, but the tests below cancel contexts that a
// ctx.Done() watcher goroutine reacts to concurrently with the test's own
// reads — exactly what -race exists to catch.
type syncEvictPool struct {
	mu      sync.Mutex
	live    []execpool.LiveExecutor
	tickets *execpool.TicketRegistry
	evicted map[string]bool
}

func newSyncEvictPool(live ...execpool.LiveExecutor) *syncEvictPool {
	return &syncEvictPool{
		live:    live,
		tickets: execpool.NewTicketRegistry(),
		evicted: make(map[string]bool),
	}
}

func (p *syncEvictPool) Live() []execpool.LiveExecutor { return p.live }

func (p *syncEvictPool) ClientFor(string) (tools.ExecutorClient, error) {
	return &stubExecutorClient{}, nil
}

func (p *syncEvictPool) Tickets() *execpool.TicketRegistry { return p.tickets }

func (p *syncEvictPool) Evict(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evicted[id] = true
}

func (p *syncEvictPool) wasEvicted(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evicted[id]
}

// waitGroupDone reports whether wg reached zero within timeout, instead of
// blocking a test forever if a watcher goroutine were ever left behind.
func waitGroupDone(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestExecutorSessionTransientEvictedWhenContextCancelled is the core
// lifetime rule Task 2.6 re-anchors: a transient session executor is
// released when its ctx ends, not when any particular connection type does
// something. This drives Controller.executorSession directly, the shared
// core behind both the framed conn-keyed path and Connect's per-stream path.
func TestExecutorSessionTransientEvictedWhenContextCancelled(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	ctx, cancel := context.WithCancel(context.Background())

	got, err := c.executorSession(ctx, "key-1", users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m1"})
	ck.NoError(err)
	ck.True(got.RunLocal, "with no durable executor the client must serve one itself")

	cancel()
	ck.True(waitGroupDone(&c.sessionExecWg, 2*time.Second), "the ctx.Done() watcher never exited after cancel")
	ck.True(pool.wasEvicted(got.ExecutorID), "cancelling ctx must evict the transient executor %s", got.ExecutorID)
}

// TestExecutorSessionDurableAnswerDoesNotEvictOnCancel: when a durable
// executor already answers, executorSession registers nothing against key or
// ctx — there is nothing transient to evict, so cancelling ctx afterward must
// not touch the durable row.
func TestExecutorSessionDurableAnswerDoesNotEvictOnCancel(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := newSyncEvictPool(liveExecutor("durable-1", map[string]string{
		"owner": "brent", "machine": "m1",
	}))
	c := &Controller{execPool: pool}
	ctx, cancel := context.WithCancel(context.Background())

	got, err := c.executorSession(ctx, "key-1", users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m1"})
	ck.NoError(err)
	ck.False(got.RunLocal || got.ExecutorID != "durable-1", "got %+v, want the durable executor answer", got)

	cancel()
	ck.True(waitGroupDone(&c.sessionExecWg, 2*time.Second), "no watcher should have been spawned for a durable answer")
	ck.False(pool.wasEvicted("durable-1"), "a durable executor must never be evicted by a session ctx ending")
}

// TestExecutorSessionConcurrentSessionsFromOneIdentityDoNotEvictEachOther
// pins the invariant the brief calls out by name: two live sessions for the
// same identity are keyed independently (a real connection for the framed
// path, a fresh ctx per call for Connect), so ending one must not touch the
// other. This is deliberate, not an oversight — see connectExecutorSessions.Open's
// doc comment for why a fresh key per Connect call is what makes it hold.
func TestExecutorSessionConcurrentSessionsFromOneIdentityDoNotEvictEachOther(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	id := users.Identity{Username: "brent"}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	first, err := c.executorSession(ctx1, "conn-1", id, protocol.ExecutorSessionRequest{Name: "m1"})
	ck.NoError(err)
	second, err := c.executorSession(ctx2, "conn-2", id, protocol.ExecutorSessionRequest{Name: "m1"})
	ck.NoError(err)
	ck.NotEq(second.ExecutorID, first.ExecutorID, "distinct keys must mint distinct executors, not share one")

	cancel1()
	// Poll rather than wait on c.sessionExecWg: session 2's watcher is still
	// parked on ctx2, so the group never reaches zero here.
	deadline := time.Now().Add(2 * time.Second)
	for !pool.wasEvicted(first.ExecutorID) {
		ck.False(time.Now().After(deadline), "session 1 (%s) was never evicted after its own ctx cancelled", first.ExecutorID)
		time.Sleep(2 * time.Millisecond)
	}
	ck.False(pool.wasEvicted(second.ExecutorID), "cancelling session 1's ctx must not evict session 2's executor %s", second.ExecutorID)

	cancel2()
	ck.True(waitGroupDone(&c.sessionExecWg, 2*time.Second), "session 2's watcher never exited after its own ctx cancelled")
	ck.True(pool.wasEvicted(second.ExecutorID), "session 2 must still evict on its own ctx ending")
}

// TestExecutorSessionConnectOwnerComesFromIdentityNotRequest: the request
// proto carries no owner-shaped field at all, and this pins why — the
// adapter must read the caller's identity off ctx, never anything in req,
// even when req names roots and a machine an attacker fully controls.
func TestExecutorSessionConnectOwnerComesFromIdentityNotRequest(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	a := connectExecutorSessions{c: c}

	ctx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u_1", Username: "brent",
	})
	ready, err := a.Open(ctx, &rafikiv1.ExecutorSessionRequest{
		Name:  "attacker-box",
		Roots: []string{"/etc"},
	})
	ck.NoError(err)
	ck.Eq("owner=brent,machine=attacker-box", ready.GetSelector(), "selector")
}

// TestExecutorSessionConnectNilIdentityFallsBackToUDSTrust: an absent
// identity is the unix socket's local trust, the same rule sessionOwner
// documents — the owner becomes the daemon's own OS user, never empty and
// never attacker-suppliable.
func TestExecutorSessionConnectNilIdentityFallsBackToUDSTrust(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	a := connectExecutorSessions{c: c}

	wantOwner, err := osUser()
	if err != nil {
		t.Skipf("no OS user available in this environment: %v", err)
	}

	ready, err := a.Open(context.Background(), &rafikiv1.ExecutorSessionRequest{Name: "m1"})
	ck.NoError(err)
	ck.Eq("owner="+wantOwner+",machine=m1", ready.GetSelector(), "selector = %q, want owner=%s,machine=m1", ready.GetSelector(), wantOwner)
}
