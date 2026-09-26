package main

import (
	"context"
	"strings"
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
	c := newSessionTestController(t, liveExecutor("durable-1", map[string]string{
		"owner":   "brent",
		"machine": "m-abc",
	}))

	got, err := c.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc", Roots: []string{"/src"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunLocal {
		t.Fatal("a durable executor already covers this machine; starting a " +
			"transient one as well offers a second executor nothing will prefer")
	}
	if got.ExecutorID != "durable-1" {
		t.Fatalf("ExecutorID = %q, want durable-1", got.ExecutorID)
	}
	if got.Selector != "owner=brent,machine=m-abc" {
		t.Fatalf("the selector must name the machine, not the hostname: %q", got.Selector)
	}
}

func TestExecutorSessionIgnoresAnotherOwnersExecutorOnTheSameName(t *testing.T) {
	c := newSessionTestController(t, liveExecutor("sams-laptop", map[string]string{
		"owner":   "sam",
		"machine": "laptop",
	}))

	got, err := c.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.RunLocal {
		t.Fatal("sam's laptop is not brent's; the durable match must be scoped " +
			"to the owner or a client binds children onto another operator's box")
	}
}

func TestExecutorSessionMintsATicketWhenNoDurableExecutorExists(t *testing.T) {
	c := newSessionTestController(t)

	got, err := c.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc", Roots: []string{"/src"}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.RunLocal {
		t.Fatal("with no durable executor the client must serve one itself")
	}
	if got.Ticket == "" {
		t.Fatal("a transient executor authenticates with a ticket")
	}
	if got.ExecutorID == "" {
		t.Fatal("the daemon assigns the id; the executor must never choose its own")
	}
	if got.Selector != "owner=brent,machine=m-abc" {
		t.Fatalf("%s", "selector must match the durable case so a child can move "+
			"between them without its stored selector changing; got "+got.Selector)
	}
}

func TestExecutorSessionSelectorIsIdenticalInBothCases(t *testing.T) {
	// This is what makes failover expressible INSIDE the confinement rules:
	// one stored selector names both executors, so effectiveExecutorSet can
	// hand a child the other one without the selector ever being rewritten.
	withDurable := newSessionTestController(t, liveExecutor("durable-1", map[string]string{
		"owner": "brent", "machine": "m-abc",
	}))
	without := newSessionTestController(t)

	a, err := withDurable.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := without.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Selector != b.Selector {
		t.Fatalf("selectors diverge: %q vs %q", a.Selector, b.Selector)
	}
}

func TestExecutorSessionRequiresAName(t *testing.T) {
	c := newSessionTestController(t)
	_, err := c.ExecutorSession(nil, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{})
	if err == nil {
		t.Fatal("without a name the daemon cannot tell which durable executor " +
			"shares this client's filesystem")
	}
	if !strings.Contains(err.Error(), "rafiki executor name") {
		t.Fatalf("the error must tell the operator how to fix it, got: %v", err)
	}
}

// fakeConn is a control.Connection stub for testing ExecutorSession
type fakeConn struct{}

func (fakeConn) Deliver([]byte)           {}
func (fakeConn) Identity() users.Identity { return users.Identity{} }
func (fakeConn) Restricted() bool         { return false }

// TestASecondSessionRequestReleasesTheFirst verifies M5: a second
// ctrl_executor_session on the same connection releases the first. Before
// this fix, the map assignment overwrote silently: the incumbent's ticket
// was never revoked and its executor never evicted.
func TestASecondSessionRequestReleasesTheFirst(t *testing.T) {
	pool := &fakePool{evicted: make(map[string]bool)}
	c := &Controller{execPool: pool}
	conn := &fakeConn{}

	first, err := c.ExecutorSession(conn, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ExecutorSession(conn, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutorID == second.ExecutorID {
		t.Fatal("each request mints a distinct executor")
	}
	if !pool.evicted[first.ExecutorID] {
		t.Fatalf("the incumbent %s was orphaned: Evict was never called", first.ExecutorID)
	}

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
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	ctx, cancel := context.WithCancel(context.Background())

	got, err := c.executorSession(ctx, "key-1", users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.RunLocal {
		t.Fatal("with no durable executor the client must serve one itself")
	}

	cancel()
	if !waitGroupDone(&c.sessionExecWg, 2*time.Second) {
		t.Fatal("the ctx.Done() watcher never exited after cancel")
	}
	if !pool.wasEvicted(got.ExecutorID) {
		t.Fatalf("cancelling ctx must evict the transient executor %s", got.ExecutorID)
	}
}

// TestExecutorSessionDurableAnswerDoesNotEvictOnCancel: when a durable
// executor already answers, executorSession registers nothing against key or
// ctx — there is nothing transient to evict, so cancelling ctx afterward must
// not touch the durable row.
func TestExecutorSessionDurableAnswerDoesNotEvictOnCancel(t *testing.T) {
	pool := newSyncEvictPool(liveExecutor("durable-1", map[string]string{
		"owner": "brent", "machine": "m1",
	}))
	c := &Controller{execPool: pool}
	ctx, cancel := context.WithCancel(context.Background())

	got, err := c.executorSession(ctx, "key-1", users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunLocal || got.ExecutorID != "durable-1" {
		t.Fatalf("got %+v, want the durable executor answer", got)
	}

	cancel()
	if !waitGroupDone(&c.sessionExecWg, 2*time.Second) {
		t.Fatal("no watcher should have been spawned for a durable answer")
	}
	if pool.wasEvicted("durable-1") {
		t.Fatal("a durable executor must never be evicted by a session ctx ending")
	}
}

// TestExecutorSessionConcurrentSessionsFromOneIdentityDoNotEvictEachOther
// pins the invariant the brief calls out by name: two live sessions for the
// same identity are keyed independently (a real connection for the framed
// path, a fresh ctx per call for Connect), so ending one must not touch the
// other. This is deliberate, not an oversight — see connectExecutorSessions.Open's
// doc comment for why a fresh key per Connect call is what makes it hold.
func TestExecutorSessionConcurrentSessionsFromOneIdentityDoNotEvictEachOther(t *testing.T) {
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	id := users.Identity{Username: "brent"}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	first, err := c.executorSession(ctx1, "conn-1", id, protocol.ExecutorSessionRequest{Name: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.executorSession(ctx2, "conn-2", id, protocol.ExecutorSessionRequest{Name: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutorID == second.ExecutorID {
		t.Fatal("distinct keys must mint distinct executors, not share one")
	}

	cancel1()
	// Poll rather than wait on c.sessionExecWg: session 2's watcher is still
	// parked on ctx2, so the group never reaches zero here.
	deadline := time.Now().Add(2 * time.Second)
	for !pool.wasEvicted(first.ExecutorID) {
		if time.Now().After(deadline) {
			t.Fatalf("session 1 (%s) was never evicted after its own ctx cancelled", first.ExecutorID)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if pool.wasEvicted(second.ExecutorID) {
		t.Fatalf("cancelling session 1's ctx must not evict session 2's executor %s", second.ExecutorID)
	}

	cancel2()
	if !waitGroupDone(&c.sessionExecWg, 2*time.Second) {
		t.Fatal("session 2's watcher never exited after its own ctx cancelled")
	}
	if !pool.wasEvicted(second.ExecutorID) {
		t.Fatal("session 2 must still evict on its own ctx ending")
	}
}

// TestExecutorSessionFramedConnectionCloseEvictsTransientExecutor is the framed path's end
// to end version of the same rule: OnConnectionClose no longer releases the
// executor directly, it cancels the connection's session context, and that
// cancellation is what the ctx.Done() watcher reacts to.
func TestExecutorSessionFramedConnectionCloseEvictsTransientExecutor(t *testing.T) {
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool, cm: newChildManager()}
	conn := &fakeConn{}

	got, err := c.ExecutorSession(conn, users.Identity{Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}

	c.OnConnectionClose(conn)

	if !waitGroupDone(&c.sessionExecWg, 2*time.Second) {
		t.Fatal("the session watcher never exited after OnConnectionClose")
	}
	if !pool.wasEvicted(got.ExecutorID) {
		t.Fatalf("OnConnectionClose must evict %s via its cancelled session context", got.ExecutorID)
	}
}

// TestExecutorSessionFramedOnConnectionCloseWithoutSessionIsANoOp: a connection that never
// called ExecutorSession has no entry in connSessions; closing it must not
// panic or touch the pool.
func TestExecutorSessionFramedOnConnectionCloseWithoutSessionIsANoOp(t *testing.T) {
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool, cm: newChildManager()}
	conn := &fakeConn{}

	c.OnConnectionClose(conn)
}

// TestExecutorSessionConnectOwnerComesFromIdentityNotRequest: the request
// proto carries no owner-shaped field at all, and this pins why — the
// adapter must read the caller's identity off ctx, never anything in req,
// even when req names roots and a machine an attacker fully controls.
func TestExecutorSessionConnectOwnerComesFromIdentityNotRequest(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetSelector() != "owner=brent,machine=attacker-box" {
		t.Fatalf("selector = %q, want owner taken from ctx identity (brent)", ready.GetSelector())
	}
}

// TestExecutorSessionConnectNilIdentityFallsBackToUDSTrust: an absent
// identity is the unix socket's local trust, the same rule sessionOwner
// documents for the framed path — the owner becomes the daemon's own OS
// user, never empty and never attacker-suppliable.
func TestExecutorSessionConnectNilIdentityFallsBackToUDSTrust(t *testing.T) {
	pool := newSyncEvictPool()
	c := &Controller{execPool: pool}
	a := connectExecutorSessions{c: c}

	wantOwner, err := osUser()
	if err != nil {
		t.Skipf("no OS user available in this environment: %v", err)
	}

	ready, err := a.Open(context.Background(), &rafikiv1.ExecutorSessionRequest{Name: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetSelector() != "owner="+wantOwner+",machine=m1" {
		t.Fatalf("selector = %q, want owner=%s,machine=m1", ready.GetSelector(), wantOwner)
	}
}
