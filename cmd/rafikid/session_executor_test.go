package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func fakeExecutor(labels, selfReported map[string]string) executors.Executor {
	return executors.Executor{
		ID:           "e_existing",
		Labels:       labels,
		SelfReported: selfReported,
		Enabled:      true,
	}
}

// The daemon writes every field that gates access. A client asking for a
// session executor supplies a name and some roots; it must not be able to
// influence owner, admits, isolation or workspace mode, because each of those
// decides who may run what where.
func TestExecutorSessionRowIsWrittenByTheDaemon(t *testing.T) {
	store := newFakeExecStore()
	c := &Controller{execStore: store}

	resp, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop", Roots: []string{"/home/brent/src"}},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if resp.Credential == "" {
		t.Fatal("no credential returned; the client cannot connect")
	}
	if resp.ExecutorID == "" {
		t.Fatal("no executorId returned; the client has nothing to target")
	}

	got := store.lastCreate
	if got.Labels["owner"] != "brent" {
		t.Errorf("owner label = %q, want the authenticated username", got.Labels["owner"])
	}
	if got.Labels["kind"] != "client" {
		t.Errorf("kind label = %q, want \"client\"", got.Labels["kind"])
	}
	if got.Labels["name"] != "laptop" {
		t.Errorf("name label = %q, want the reported name", got.Labels["name"])
	}
	if got.Isolation != "none" {
		t.Errorf("isolation = %q, want \"none\"", got.Isolation)
	}
	if got.WorkspaceMode != "pinned" {
		t.Errorf("workspaceMode = %q, want \"pinned\" — an operator's own machine is not interchangeable", got.WorkspaceMode)
	}
	if got.Admits != "owner=brent" {
		t.Errorf("admits = %q, want owner=brent so nobody else's children land here", got.Admits)
	}
}

// The UDS fallback: with a zero identity the owner is the current OS user.
func TestExecutorSessionUDSOwnerFallback(t *testing.T) {
	store := newFakeExecStore()
	c := &Controller{execStore: store}

	resp, err := c.ExecutorSession(
		users.Identity{}, // zero identity — UDS connection
		protocol.ExecutorSessionRequest{Name: "laptop"},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if resp.Credential == "" {
		t.Fatal("no credential returned for UDS session")
	}
	if got := store.lastCreate.Labels["owner"]; got == "" {
		t.Fatal("owner label is empty; the UDS fallback did not resolve an owner")
	}
}

// When a persistent executor already covers this name and owner, the client
// must NOT start a second one: the persistent row survives the client exiting,
// which is what lets an agent keep working after the operator detaches.
func TestExecutorSessionDefersToAnExistingExecutor(t *testing.T) {
	store := newFakeExecStore()
	store.execs["e_existing"] = fakeExecutor(
		map[string]string{"owner": "brent"},
		map[string]string{"name": "laptop"}, // SelfReported, not Labels
	)
	c := &Controller{execStore: store}

	resp, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if resp.RunLocal {
		t.Error("RunLocal is true though a durable executor already covers this name")
	}
	if resp.Credential != "" {
		t.Error("a credential was issued though a persistent executor already covers this name")
	}
	if resp.ExecutorID == "" {
		t.Error("no executorId returned; the client has nothing to target")
	}
	if store.createCalls != 0 {
		t.Errorf("Create was called %d times; want 0", store.createCalls)
	}
}

// A client that already holds a credential must not cause another row. Every
// mint is permanent — executors.Store has SetEnabled and no Delete — so getting
// this wrong grows the registry by one row per invocation, forever.
func TestExecutorSessionDoesNotMintWhenTheClientHasACredential(t *testing.T) {
	store := newFakeExecStore()
	c := &Controller{execStore: store}

	resp, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop", HasCredential: true},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if store.createCalls != 0 {
		t.Errorf("Create was called %d times; want 0", store.createCalls)
	}
	if !resp.RunLocal {
		t.Error("RunLocal is false; the client holds a credential and should serve")
	}
	if resp.Credential != "" {
		t.Error("a second credential was issued")
	}
}

// Minting while an enabled client row exists means the client lost its
// credential: only the hash was stored, so that row is unusable forever.
func TestExecutorSessionDisablesAnUnusableClientRow(t *testing.T) {
	store := newFakeExecStore()
	store.execs["stale"] = fakeExecutor(
		map[string]string{"owner": "brent", "name": "laptop", "kind": "client"},
		nil,
	)
	c := &Controller{execStore: store}

	if _, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop"},
	); err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if store.createCalls != 1 {
		t.Errorf("Create was called %d times; want 1 — a stale client row must not block a new one", store.createCalls)
	}
	if len(store.disabled) != 1 {
		t.Errorf("disabled %d rows; want the one unusable client row", len(store.disabled))
	}
}

// Two concurrent local `rafiki` processes on the same machine (e.g. `create`
// still attached in one terminal, `attach` from a second) share the one
// per-machine credential file and both ask for a session. The SECOND must
// defer to the first's already-live connection rather than being told
// RunLocal and going on to lose the pool's one-connection-per-credential
// admission race — which is what produced the endless "another connection
// for this executor is already live" reconnect loop this test guards against.
func TestExecutorSessionDefersToAnAlreadyLiveClientExecutor(t *testing.T) {
	store := newFakeExecStore()
	pool := &fakePool{live: []execpool.LiveExecutor{
		{Executor: fakeExecutor(
			map[string]string{"owner": "brent", "kind": "client"},
			map[string]string{"name": "laptop"},
		)},
	}}
	c := &Controller{execStore: store, execPool: pool}

	resp, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop", HasCredential: true},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if resp.RunLocal {
		t.Error("RunLocal is true though this owner+name already has a live client executor")
	}
	if resp.ExecutorID != "e_existing" {
		t.Errorf("ExecutorID = %q, want the already-live executor's id", resp.ExecutorID)
	}
	if store.createCalls != 0 {
		t.Errorf("Create was called %d times; want 0", store.createCalls)
	}
}

// A kind=client row that is enabled in the store but NOT currently connected
// (the previous session ended) must not short-circuit RunLocal — nameAlreadyCovered
// already excludes kind=client for the durable case, and liveClientExecutor must
// only match the POOL's live set, not the store, or no session would ever
// actually (re)connect.
func TestExecutorSessionRunsLocalWhenNoClientExecutorIsLive(t *testing.T) {
	store := newFakeExecStore()
	pool := &fakePool{} // nothing live
	c := &Controller{execStore: store, execPool: pool}

	resp, err := c.ExecutorSession(
		users.Identity{UserID: "u_1", Username: "brent"},
		protocol.ExecutorSessionRequest{Name: "laptop", HasCredential: true},
	)
	if err != nil {
		t.Fatalf("ExecutorSession: %v", err)
	}
	if !resp.RunLocal {
		t.Error("RunLocal is false though no client executor is live for this owner+name")
	}
}

// The client cannot synthesise a selector for itself — it does not know the
// owner, which the daemon derives from the connection — so the response must
// carry one. Without it plan 07 has nothing to put on ExecutorSelector and the
// spawn cannot find the row.
func TestExecutorSessionReturnsAUsableSelector(t *testing.T) {
	c := &Controller{execStore: newFakeExecStore()}
	resp, err := c.ExecutorSession(users.Identity{Username: "brent"}, protocol.ExecutorSessionRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Selector == "" {
		t.Fatal("Selector is empty; the client has nothing to target")
	}
	if resp.Selector != "owner=brent,kind=client" {
		t.Errorf("Selector = %q, want the minted client row's unique selector", resp.Selector)
	}
}
