// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/server"
)

// newGitAdapterFixture wires a connectGitSources over the git pusher fixture:
// a real pusher (so the adapter's synchronous first refresh is exercised) and
// a real in-memory store.
func newGitAdapterFixture() (*gitSourceFixture, connectGitSources) {
	f := newGitSourceFixture()
	return f, connectGitSources{c: &Controller{gitpymoduleStore: f.store, gitpymodulePusher: f.gp}}
}

// TestConnectGitSourcesOwnerFromContext pins the security-relevant rule, the
// same one TestConnectPyModulesOwnerFromContext pins for blob modules: the
// owner is resolved from the request CONTEXT, never from a request field —
// the anonymous unix-socket caller writes the shared unattributed bucket and
// an identified caller writes (and lists) only its own.
func TestConnectGitSourcesOwnerFromContext(t *testing.T) {
	f, m := newGitAdapterFixture()

	// No identity on the ctx: the anonymous unix-socket case. With no pusher
	// eligible executors for the unattributed owner, the synchronous first
	// refresh fails — but the row must still be registered, so exercise the
	// write through RemoveGitSource's counterpart instead: register, then
	// list, expecting the row in the empty-owner bucket.
	if _, err := f.store.Put(context.Background(), "", "anon_lib", "https://example.net/anon.git", "main"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := m.ListGitSources(context.Background())
	if err != nil {
		t.Fatalf("ListGitSources with no identity: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "anon_lib" {
		t.Fatalf("anonymous list = %+v, want only anon_lib (the unattributed bucket)", rows)
	}

	// An identified ctx: writes and reads land in THAT identity's bucket.
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	rows, err = m.ListGitSources(alice)
	if err != nil {
		t.Fatalf("ListGitSources as u_alice: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("alice's list = %+v, want her two seeded sources", rows)
	}

	// bob's ctx sees bob's rows only.
	bob := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_bob"})
	rows, err = m.ListGitSources(bob)
	if err != nil {
		t.Fatalf("ListGitSources as u_bob: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "bob_lib" {
		t.Fatalf("bob's list = %+v, want only [bob_lib]", rows)
	}

	// Removing is owner-scoped too: bob can delete bob_lib and nothing else.
	if err := m.RemoveGitSource(bob, "bob_lib"); err != nil {
		t.Fatalf("RemoveGitSource as u_bob: %v", err)
	}
	if len(f.store.deleted) != 1 || f.store.deleted[0] != [2]string{"u_bob", "bob_lib"} {
		t.Fatalf("deletes = %v, want exactly [u_bob bob_lib]", f.store.deleted)
	}
	if err := m.RemoveGitSource(bob, "ops_tools"); err != gitpymodules.ErrNotFound {
		t.Fatalf("RemoveGitSource of alice's source as bob = %v, want ErrNotFound", err)
	}
}

// TestConnectGitSourcesAddRefreshesFirstAndRefreshReusesStored pins the two
// non-trivial adapter behaviors: AddGitSource fires a synchronous refresh
// with the request's url/ref (so `python repo add` fails loudly on a bad
// clone instead of returning an empty list), and RefreshGitSource re-sends
// the STORED url/ref — a later Put repoints the source, and a refresh must
// follow the registration, not a stale caller-supplied pair.
func TestConnectGitSourcesAddRefreshesFirstAndRefreshReusesStored(t *testing.T) {
	f, m := newGitAdapterFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncPyModuleGitSourceResponse{VenvReady: true}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/fresh.git", "develop")
	if err != nil {
		t.Fatalf("AddGitSource: %v", err)
	}
	if row.Name != "fresh_tools" || row.URL != "https://example.net/fresh.git" || row.Ref != "develop" {
		t.Fatalf("AddGitSource row = %+v", row)
	}
	// The first refresh went out synchronously, to alice's executor only.
	if got := f.clients["exec-alice"].callCount(); got != 1 {
		t.Fatalf("exec-alice received %d refresh RPC(s) after add, want 1", got)
	}
	if got := f.clients["exec-bob"].callCount(); got != 0 {
		t.Errorf("exec-bob received %d refresh RPC(s) after alice's add, want 0", got)
	}

	// Repoint the source, then refresh: the executor must be told the NEW
	// url/ref from the store, not the add-time pair.
	if _, err := f.store.Put(alice, "u_alice", "fresh_tools", "https://example.net/moved.git", "release"); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	scripts, packages, venvReady, venvError, err := m.RefreshGitSource(alice, "fresh_tools")
	if err != nil {
		t.Fatalf("RefreshGitSource: %v", err)
	}
	if len(scripts) != 0 || len(packages) != 0 || !venvReady || venvError != "" {
		t.Errorf("RefreshGitSource = %v/%v ready=%v err=%q, want the empty default executor answer", scripts, packages, venvReady, venvError)
	}
	if got := f.clients["exec-alice"].callCount(); got != 2 {
		t.Fatalf("exec-alice received %d refresh RPC(s) after refresh, want 2", got)
	}
	req := f.clients["exec-alice"].requests[1]
	if req.GetUrl() != "https://example.net/moved.git" || req.GetRef() != "release" || req.GetName() != "fresh_tools" {
		t.Errorf("refresh payload = name %q url %q ref %q, want the STORED registration", req.GetName(), req.GetUrl(), req.GetRef())
	}

	// An unknown name is the store's ErrNotFound, not a silent empty refresh.
	if _, _, _, _, err := m.RefreshGitSource(alice, "no_such_source"); err != gitpymodules.ErrNotFound {
		t.Fatalf("RefreshGitSource(unknown) = %v, want ErrNotFound", err)
	}
}

// TestConnectGitSourcesAddReturnsRefreshErrorAndKeepsRow pins what a failing
// first refresh does to a registration: the add fails loudly with the refresh
// error — the operator hears about the bad clone immediately — and the row
// still survives in the store, so the retry is `python repo refresh`, not a
// second add.
func TestConnectGitSourcesAddReturnsRefreshErrorAndKeepsRow(t *testing.T) {
	f, m := newGitAdapterFixture()
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	if _, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/fresh.git", "develop"); err == nil {
		t.Fatal("AddGitSource with a failing first refresh: succeeded, want the refresh error")
	}

	rows, err := m.ListGitSources(alice)
	if err != nil {
		t.Fatalf("ListGitSources after the failed add: %v", err)
	}
	survived := false
	for _, r := range rows {
		if r.Name == "fresh_tools" {
			survived = r.URL == "https://example.net/fresh.git" && r.Ref == "develop"
		}
	}
	if !survived {
		t.Fatalf("rows after a failed first refresh = %+v, want fresh_tools to have survived the failed refresh", rows)
	}
}

// TestConnectGitSourcesNilPusherStillRegisters pins the nil guard: a daemon
// with a database but no executor pool constructs no pusher, and registering
// must still succeed — the write is the durable part; there is just nothing
// to refresh on. A refresh without a pool fails with a named error rather
// than panicking or pretending.
func TestConnectGitSourcesNilPusherStillRegisters(t *testing.T) {
	f := newGitSourceFixture()
	m := connectGitSources{c: &Controller{gitpymoduleStore: f.store}} // pusher deliberately nil
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, err := m.AddGitSource(alice, "solo_tools", "https://example.net/solo.git", "main")
	if err != nil {
		t.Fatalf("AddGitSource with nil pusher: %v", err)
	}
	if row.Name != "solo_tools" || row.URL != "https://example.net/solo.git" {
		t.Errorf("AddGitSource row = %+v", row)
	}
	if _, _, _, _, err := m.RefreshGitSource(alice, "solo_tools"); err == nil {
		t.Fatal("RefreshGitSource with nil pusher: succeeded, want a named error")
	}
	if err := m.RemoveGitSource(alice, "solo_tools"); err != nil {
		t.Fatalf("RemoveGitSource with nil pusher: %v", err)
	}
}

// TestGitPymoduleRemoveEvictsCachedInventory pins the eviction on remove: the
// pusher's cache is read without consulting the store (allInventory for the
// skill body and MCP pymodule_list, inventoryFor for `python list --repo
// <name>`), so a source removed without eviction keeps rendering its rows on
// every span surface until the daemon restarts. After a remove, the removed
// name is gone from both readers while an unrelated source's entry survives.
func TestGitPymoduleRemoveEvictsCachedInventory(t *testing.T) {
	f, m := newGitAdapterFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// Populate the cache the way real traffic does: the adapter's synchronous
	// first refresh on add, for two of alice's sources.
	if _, err := m.AddGitSource(alice, "ops_tools", "https://example.net/ops.git", "main"); err != nil {
		t.Fatalf("AddGitSource ops_tools: %v", err)
	}
	if _, err := m.AddGitSource(alice, "shared_lib", "https://example.net/lib.git", "v2"); err != nil {
		t.Fatalf("AddGitSource shared_lib: %v", err)
	}
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); !ok {
		t.Fatal("precondition: ops_tools was never cached")
	}

	if err := m.RemoveGitSource(alice, "ops_tools"); err != nil {
		t.Fatalf("RemoveGitSource: %v", err)
	}

	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok {
		t.Error("inventoryFor still reports a removed source; its rows would keep rendering on `python list --repo ops_tools`")
	}
	all := f.gp.allInventory("u_alice")
	if _, ok := all["ops_tools"]; ok {
		t.Error("allInventory still reports the removed source; its rows would keep rendering on every span surface")
	}
	if _, ok := all["shared_lib"]; !ok {
		t.Error("allInventory lost shared_lib, which was NOT removed")
	}
	if _, ok := f.gp.inventoryFor("u_alice", "shared_lib"); !ok {
		t.Error("inventoryFor lost shared_lib, which was NOT removed")
	}
}
