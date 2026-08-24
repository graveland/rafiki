package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
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

func (fakeConn) Deliver([]byte)        {}
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
