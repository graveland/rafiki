// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
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
	c.NoError(err, "ListGitSources with no identity")
	c.False(len(rows) != 1 || rows[0].Name != "anon_lib", "anonymous list = %+v, want only anon_lib (the unattributed bucket)", rows)

	// An identified ctx: writes and reads land in THAT identity's bucket.
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	rows, err = m.ListGitSources(alice)
	c.NoError(err, "ListGitSources as u_alice")
	c.Len(rows, 2, "alice's list")

	// bob's ctx sees bob's rows only.
	bob := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_bob"})
	rows, err = m.ListGitSources(bob)
	c.NoError(err, "ListGitSources as u_bob")
	c.False(len(rows) != 1 || rows[0].Name != "bob_lib", "bob's list = %+v, want only [bob_lib]", rows)

	// Removing is owner-scoped too: bob can delete bob_lib and nothing else.
	c.NoError(m.RemoveGitSource(bob, "bob_lib"), "RemoveGitSource as u_bob")
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
	c := assert.NewCollecting(t)
	f, m := newGitAdapterFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncPyModuleGitSourceResponse{VenvReady: true}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/fresh.git", "develop")
	c.Require().NoError(err, "AddGitSource")
	c.Require().False(row.Name != "fresh_tools" || row.URL != "https://example.net/fresh.git" || row.Ref != "develop", "AddGitSource row = %+v", row)
	// The first refresh went out synchronously, to alice's executor only.
	c.Require().Eq(1, f.clients["exec-alice"].callCount(), "exec-alice received")
	c.Eq(0, f.clients["exec-bob"].callCount(), "exec-bob received")

	// Repoint the source, then refresh: the executor must be told the NEW
	// url/ref from the store, not the add-time pair.
	if _, err := f.store.Put(alice, "u_alice", "fresh_tools", "https://example.net/moved.git", "release"); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	scripts, packages, venvReady, venvError, err := m.RefreshGitSource(alice, "fresh_tools")
	c.Require().NoError(err, "RefreshGitSource")
	c.False(len(scripts) != 0 || len(packages) != 0 || !venvReady || venvError != "", "RefreshGitSource = %v/%v ready=%v err=%q, want the empty default executor answer", scripts, packages, venvReady, venvError)
	c.Require().Eq(2, f.clients["exec-alice"].callCount(), "exec-alice received")
	req := f.clients["exec-alice"].requests[1]
	if req.GetUrl() != "https://example.net/moved.git" || req.GetRef() != "release" || req.GetName() != "fresh_tools" {
		t.Errorf("refresh payload = name %q url %q ref %q, want the STORED registration", req.GetName(), req.GetUrl(), req.GetRef())
	}

	// An unknown name is the store's ErrNotFound, not a silent empty refresh.
	if _, _, _, _, err := m.RefreshGitSource(alice, "no_such_source"); err != gitpymodules.ErrNotFound {
		t.Fatalf("RefreshGitSource(unknown) = %v, want ErrNotFound", err)
	}
}

// TestGitSourceAddRollsBackNewOnFailedRefresh: a failed FIRST refresh on a
// brand-new name fails the add AND undoes the registration — the store is
// left without the row, exactly as before the call, via the store's own
// Delete (that table is a pointer to repoint, not an append-only history),
// and the caller gets the refresh error, never a rollback error masking it.
func TestGitSourceAddRollsBackNewOnFailedRefresh(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newGitAdapterFixture()
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/fresh.git", "develop")
	c.Require().Error(err, "AddGitSource with a failing first refresh")
	c.False(row.Name != "" || row.URL != "" || row.Ref != "", "AddGitSource row = %+v, want the zero row on failure", row)
	// The add's error is the refresh error — not a rollback failure masking it.
	c.StrContains(err.Error(), "every eligible executor failed", "AddGitSource's error")

	rows, err := m.ListGitSources(alice)
	c.Require().NoError(err, "ListGitSources after the failed add")
	c.Len(rows, 2, "rows after the failed add (the two pre-existing sources only)")
	for _, r := range rows {
		c.NotEq("fresh_tools", r.Name, "a rolled-back registration survived the failed add")
	}
	// The undo went through the store's own Delete — and touched nothing else.
	c.Require().Len(f.store.deleted, 1, "deletes after the failed add")
	c.Eq([2]string{"u_alice", "fresh_tools"}, f.store.deleted[0], "the delete")
}

// TestGitSourceAddRestoresRepointOnFailedRefresh: a failed FIRST refresh on a
// RE-ADDED name fails the add AND restores the previous registration — the
// row is back at its old url/ref, nothing was deleted, and the caller gets
// the refresh error.
func TestGitSourceAddRestoresRepointOnFailedRefresh(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newGitAdapterFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// A successful add first: fresh_tools registers at url/ref A.
	row, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/old.git", "main")
	c.Require().NoError(err, "the first, succeeding add")
	c.Require().False(row.URL != "https://example.net/old.git" || row.Ref != "main", "first add row = %+v", row)

	// Now the clone breaks, and the source is re-added at url/ref B.
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	_, err = m.AddGitSource(alice, "fresh_tools", "https://example.net/new.git", "develop")
	c.Require().Error(err, "the re-add with a failing first refresh")
	c.StrContains(err.Error(), "every eligible executor failed", "AddGitSource's error")

	rows, err := m.ListGitSources(alice)
	c.Require().NoError(err, "ListGitSources after the failed re-add")
	survived := false
	for _, r := range rows {
		if r.Name == "fresh_tools" {
			survived = r.URL == "https://example.net/old.git" && r.Ref == "main"
		}
	}
	c.True(survived, "rows after the failed re-add = %+v, want fresh_tools restored to its PREVIOUS url/ref", rows)
	// A repoint is restored, never deleted.
	c.Len(f.store.deleted, 0, "deletes after the failed re-add")
	c.Eq(2, f.clients["exec-alice"].callCount(), "exec-alice received (once per add)")
}

// TestGitSourceAddRollsBackWhenCallerCancels: the most likely way a first
// refresh fails is the CALLER disconnecting (Ctrl-C on a long clone), which
// cancels the handler ctx — and the rollback must survive exactly that: it
// runs on context.WithoutCancel, so the bad row is still deleted when the
// fake refresh cancels ctx and then fails.
func TestGitSourceAddRollsBackWhenCallerCancels(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newGitAdapterFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := &fakeGitSourceClient{
		err: errors.New("git clone failed: context canceled"),
		// The onCall hook runs before the failure is produced: cancel the
		// request ctx first, exactly as a disconnecting caller would.
		onCall: cancel,
	}
	f.pool.clients["exec-alice"] = cl
	f.clients["exec-alice"] = cl
	alice := server.WithIdentity(ctx, &server.Identity{UserID: "u_alice"})

	_, err := m.AddGitSource(alice, "fresh_tools", "https://example.net/fresh.git", "develop")
	c.Require().Error(err, "AddGitSource whose refresh cancels ctx then fails")

	// The delete STILL happened, on the uncancelled rollback context.
	c.Require().Len(f.store.deleted, 1, "deletes after the cancelled refresh")
	c.Eq([2]string{"u_alice", "fresh_tools"}, f.store.deleted[0], "the delete")
	rows, lerr := m.ListGitSources(context.Background())
	c.Require().NoError(lerr, "ListGitSources after the cancelled refresh")
	for _, r := range rows {
		c.NotEq("fresh_tools", r.Name, "a rolled-back registration survived a caller-cancelled add")
	}
}

// TestConnectGitSourcesNilPusherStillRegisters pins the nil guard: a daemon
// with a database but no executor pool constructs no pusher, and registering
// must still succeed — the write is the durable part; there is just nothing
// to refresh on. A refresh without a pool fails with a named error rather
// than panicking or pretending.
func TestConnectGitSourcesNilPusherStillRegisters(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newGitSourceFixture()
	m := connectGitSources{c: &Controller{gitpymoduleStore: f.store}} // pusher deliberately nil
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, err := m.AddGitSource(alice, "solo_tools", "https://example.net/solo.git", "main")
	ck.Require().NoError(err, "AddGitSource with nil pusher")
	ck.False(row.Name != "solo_tools" || row.URL != "https://example.net/solo.git", "AddGitSource row = %+v", row)
	if _, _, _, _, err := m.RefreshGitSource(alice, "solo_tools"); err == nil {
		t.Fatal("RefreshGitSource with nil pusher: succeeded, want a named error")
	}
	ck.Require().NoError(m.RemoveGitSource(alice, "solo_tools"), "RemoveGitSource with nil pusher")
}

// TestGitPymoduleRemoveEvictsCachedInventory pins the eviction on remove: the
// pusher's cache is read without consulting the store (allInventory for the
// skill body and MCP pymodule_list, inventoryFor for `python list --repo
// <name>`), so a source removed without eviction keeps rendering its rows on
// every span surface until the daemon restarts. After a remove, the removed
// name is gone from both readers while an unrelated source's entry survives.
func TestGitPymoduleRemoveEvictsCachedInventory(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Require().NoError(m.RemoveGitSource(alice, "ops_tools"), "RemoveGitSource")

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
	_, ok := f.gp.inventoryFor("u_alice", "shared_lib")
	c.True(ok, "inventoryFor lost shared_lib, which was NOT removed")
}
