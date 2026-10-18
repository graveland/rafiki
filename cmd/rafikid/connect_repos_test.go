// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

// newRepoAdapterFixture wires a connectRepos over the repo pusher fixture:
// a real pusher (so the adapter's synchronous first refresh is exercised) and
// a real in-memory store.
func newRepoAdapterFixture() (*repoFixture, connectRepos) {
	f := newRepoFixture()
	return f, connectRepos{c: &Controller{repoStore: f.store, repoPusher: f.gp}}
}

// TestConnectReposOwnerFromContext pins the security-relevant rule, the
// same one TestConnectPyModulesOwnerFromContext pins for blob modules: the
// owner is resolved from the request CONTEXT, never from a request field —
// the anonymous unix-socket caller writes the shared unattributed bucket and
// an identified caller writes (and lists) only its own.
func TestConnectReposOwnerFromContext(t *testing.T) {
	c := assert.NewAborting(t)
	f, m := newRepoAdapterFixture()

	// No identity on the ctx: the anonymous unix-socket case. With no pusher
	// eligible executors for the unattributed owner, the synchronous first
	// refresh fails — but the row must still be registered, so exercise the
	// write through RemoveRepo's counterpart instead: register, then
	// list, expecting the row in the empty-owner bucket.
	if _, err := f.store.Put(context.Background(), "", "anon_lib", "https://example.net/anon.git", "main"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := m.ListRepos(context.Background())
	c.NoError(err, "ListRepos with no identity")
	c.False(len(rows) != 1 || rows[0].Name != "anon_lib", "anonymous list = %+v, want only anon_lib (the unattributed bucket)", rows)

	// An identified ctx: writes and reads land in THAT identity's bucket.
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	rows, err = m.ListRepos(alice)
	c.NoError(err, "ListRepos as u_alice")
	c.Len(rows, 2, "alice's list")

	// bob's ctx sees bob's rows only.
	bob := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_bob"})
	rows, err = m.ListRepos(bob)
	c.NoError(err, "ListRepos as u_bob")
	c.False(len(rows) != 1 || rows[0].Name != "bob_lib", "bob's list = %+v, want only [bob_lib]", rows)

	// Removing is owner-scoped too: bob can delete bob_lib and nothing else.
	c.NoError(m.RemoveRepo(bob, "bob_lib"), "RemoveRepo as u_bob")
	if len(f.store.deleted) != 1 || f.store.deleted[0] != [2]string{"u_bob", "bob_lib"} {
		t.Fatalf("deletes = %v, want exactly [u_bob bob_lib]", f.store.deleted)
	}
	if err := m.RemoveRepo(bob, "ops_tools"); err != repos.ErrNotFound {
		t.Fatalf("RemoveRepo of alice's source as bob = %v, want ErrNotFound", err)
	}
}

// TestConnectReposAddRefreshesFirstAndRefreshReusesStored pins the two
// non-trivial adapter behaviors: AddRepo fires a synchronous refresh
// with the request's url/ref (so `repo add` fails loudly on a bad
// clone instead of returning an empty list), and RefreshRepo re-sends
// the STORED url/ref — a later Put repoints the source, and a refresh must
// follow the registration, not a stale caller-supplied pair.
func TestConnectReposAddRefreshesFirstAndRefreshReusesStored(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newRepoAdapterFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{VenvReady: true}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, _, err := m.AddRepo(alice, "fresh_tools", "https://example.net/fresh.git", "develop", 0)
	c.Require().NoError(err, "AddRepo")
	c.Require().False(row.Name != "fresh_tools" || row.URL != "https://example.net/fresh.git" || row.Ref != "develop", "AddRepo row = %+v", row)
	// The first refresh went out synchronously, to alice's executor only.
	c.Require().Eq(1, f.clients["exec-alice"].callCount(), "exec-alice received")
	c.Eq(0, f.clients["exec-bob"].callCount(), "exec-bob received")

	// Repoint the source, then refresh: the executor must be told the NEW
	// url/ref from the store, not the add-time pair.
	if _, err := f.store.Put(alice, "u_alice", "fresh_tools", "https://example.net/moved.git", "release"); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	refresh, err := m.RefreshRepo(alice, "fresh_tools")
	c.Require().NoError(err, "RefreshRepo")
	c.False(len(refresh.Scripts) != 0 || len(refresh.Packages) != 0 || !refresh.VenvReady || refresh.VenvError != "",
		"RefreshRepo = %v/%v ready=%v err=%q, want the empty default executor answer", refresh.Scripts, refresh.Packages, refresh.VenvReady, refresh.VenvError)
	c.Eq(defaultResolveCommit, refresh.Commit, "RefreshRepo commit")
	c.Require().Eq(2, f.clients["exec-alice"].callCount(), "exec-alice received")
	req := f.clients["exec-alice"].requests[1]
	if req.GetUrl() != "https://example.net/moved.git" || req.GetRef() != "release" || req.GetName() != "fresh_tools" {
		t.Errorf("refresh payload = name %q url %q ref %q, want the STORED registration", req.GetName(), req.GetUrl(), req.GetRef())
	}

	// An unknown name is the store's ErrNotFound, not a silent empty refresh.
	if _, err := m.RefreshRepo(alice, "no_such_source"); err != repos.ErrNotFound {
		t.Fatalf("RefreshRepo(unknown) = %v, want ErrNotFound", err)
	}
}

// TestRepoAddRollsBackNewOnFailedRefresh: a failed FIRST refresh on a
// brand-new name fails the add AND undoes the registration — the store is
// left without the row, exactly as before the call, via the store's own
// Delete (that table is a pointer to repoint, not an append-only history),
// and the caller gets the refresh error, never a rollback error masking it.
func TestRepoAddRollsBackNewOnFailedRefresh(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newRepoAdapterFixture()
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, _, err := m.AddRepo(alice, "fresh_tools", "https://example.net/fresh.git", "develop", 0)
	c.Require().Error(err, "AddRepo with a failing first refresh")
	c.False(row.Name != "" || row.URL != "" || row.Ref != "", "AddRepo row = %+v, want the zero row on failure", row)
	// The add's error is the refresh error — not a rollback failure masking it.
	c.StrContains(err.Error(), "every eligible executor failed", "AddRepo's error")

	rows, err := m.ListRepos(alice)
	c.Require().NoError(err, "ListRepos after the failed add")
	c.Len(rows, 2, "rows after the failed add (the two pre-existing sources only)")
	for _, r := range rows {
		c.NotEq("fresh_tools", r.Name, "a rolled-back registration survived the failed add")
	}
	// The undo went through the store's own Delete — and touched nothing else.
	c.Require().Len(f.store.deleted, 1, "deletes after the failed add")
	c.Eq([2]string{"u_alice", "fresh_tools"}, f.store.deleted[0], "the delete")
}

// TestRepoAddRestoresRepointOnFailedRefresh: a failed FIRST refresh on a
// RE-ADDED name fails the add AND restores the previous registration — the
// row is back at its old url/ref, nothing was deleted, and the caller gets
// the refresh error.
func TestRepoAddRestoresRepointOnFailedRefresh(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newRepoAdapterFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// A successful add first: fresh_tools registers at url/ref A.
	row, _, err := m.AddRepo(alice, "fresh_tools", "https://example.net/old.git", "main", 0)
	c.Require().NoError(err, "the first, succeeding add")
	c.Require().False(row.URL != "https://example.net/old.git" || row.Ref != "main", "first add row = %+v", row)

	// Now the clone breaks, and the source is re-added at url/ref B.
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	_, _, err = m.AddRepo(alice, "fresh_tools", "https://example.net/new.git", "develop", 0)
	c.Require().Error(err, "the re-add with a failing first refresh")
	c.StrContains(err.Error(), "every eligible executor failed", "AddRepo's error")

	rows, err := m.ListRepos(alice)
	c.Require().NoError(err, "ListRepos after the failed re-add")
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

// TestRepoAddRollsBackWhenCallerCancels: the most likely way a first
// refresh fails is the CALLER disconnecting (Ctrl-C on a long clone), which
// cancels the handler ctx — and the rollback must survive exactly that: it
// runs on context.WithoutCancel, so the bad row is still deleted when the
// fake refresh cancels ctx and then fails.
func TestRepoAddRollsBackWhenCallerCancels(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newRepoAdapterFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := &fakeRepoClient{
		err: errors.New("git clone failed: context canceled"),
		// The onCall hook runs before the failure is produced: cancel the
		// request ctx first, exactly as a disconnecting caller would.
		onCall: cancel,
	}
	f.pool.clients["exec-alice"] = cl
	f.clients["exec-alice"] = cl
	alice := server.WithIdentity(ctx, &server.Identity{UserID: "u_alice"})

	_, _, err := m.AddRepo(alice, "fresh_tools", "https://example.net/fresh.git", "develop", 0)
	c.Require().Error(err, "AddRepo whose refresh cancels ctx then fails")

	// The delete STILL happened, on the uncancelled rollback context.
	c.Require().Len(f.store.deleted, 1, "deletes after the cancelled refresh")
	c.Eq([2]string{"u_alice", "fresh_tools"}, f.store.deleted[0], "the delete")
	rows, lerr := m.ListRepos(context.Background())
	c.Require().NoError(lerr, "ListRepos after the cancelled refresh")
	for _, r := range rows {
		c.NotEq("fresh_tools", r.Name, "a rolled-back registration survived a caller-cancelled add")
	}
}

// TestConnectReposNilPusherStillRegisters pins the nil guard: a daemon
// with a database but no executor pool constructs no pusher, and registering
// must still succeed — the write is the durable part; there is just nothing
// to refresh on. A refresh without a pool fails with a named error rather
// than panicking or pretending.
func TestConnectReposNilPusherStillRegisters(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoFixture()
	m := connectRepos{c: &Controller{repoStore: f.store}} // pusher deliberately nil
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	row, _, err := m.AddRepo(alice, "solo_tools", "https://example.net/solo.git", "main", 0)
	ck.Require().NoError(err, "AddRepo with nil pusher")
	ck.False(row.Name != "solo_tools" || row.URL != "https://example.net/solo.git", "AddRepo row = %+v", row)
	if _, err := m.RefreshRepo(alice, "solo_tools"); err == nil {
		t.Fatal("RefreshRepo with nil pusher: succeeded, want a named error")
	}
	ck.Require().NoError(m.RemoveRepo(alice, "solo_tools"), "RemoveRepo with nil pusher")
}

// TestRepoRemoveEvictsCachedInventory pins the eviction on remove: the
// pusher's cache is read without consulting the store (allInventory for the
// skill body and MCP pymodule_list, inventoryFor for `python list --repo
// <name>`), so a source removed without eviction keeps rendering its rows on
// every span surface until the daemon restarts. After a remove, the removed
// name is gone from both readers while an unrelated source's entry survives.
func TestRepoRemoveEvictsCachedInventory(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m := newRepoAdapterFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// Populate the cache the way real traffic does: the adapter's synchronous
	// first refresh on add, for two of alice's sources.
	if _, _, err := m.AddRepo(alice, "ops_tools", "https://example.net/ops.git", "main", 0); err != nil {
		t.Fatalf("AddRepo ops_tools: %v", err)
	}
	if _, _, err := m.AddRepo(alice, "shared_lib", "https://example.net/lib.git", "v2", 0); err != nil {
		t.Fatalf("AddRepo shared_lib: %v", err)
	}
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); !ok {
		t.Fatal("precondition: ops_tools was never cached")
	}

	c.Require().NoError(m.RemoveRepo(alice, "ops_tools"), "RemoveRepo")

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

// ─── the skills half, the schedule and the resolved state ────────────────────

// newRepoSkillsFixture wires a connectRepos over the repo fixture WITH a skill
// store and a real pool holding one claude-capable executor, so a write's push
// is observable. The corpus holds one enabled core row: a push of an empty
// corpus is deliberately declined, and would make "was it pushed?"
// untestable.
func newRepoSkillsFixture(t *testing.T) (*repoFixture, connectRepos, *repoSkillStore, *pushFakeExecutor) {
	t.Helper()
	f := newRepoFixture()
	sk := &repoSkillStore{
		changed: true,
		rows: []skills.Record{{
			ID: "s1", Namespace: skills.DefaultNamespace, Name: "core", Enabled: true,
			Source: skills.CoreSource,
		}},
	}
	pool, fakes := pushTestPool(t, executors.Executor{ID: "exec-skills", OwnerUserID: "u_alice"})
	pusher := &skillPusher{pool: pool, store: sk, version: "test"}
	// The refresh's skills half writes through the PUSHER's store (production
	// wires the same store into both), and pushes through the pusher — so the
	// fixture must do both, or a collision would go undetected.
	f.gp.skillStore = sk
	f.gp.skillPush = func(ctx context.Context) { pusher.pushAll(ctx) }
	// A non-nil response: the skills half reads the executor's discovered set,
	// and a nil message is a nil set.
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{}
	m := connectRepos{c: &Controller{
		repoStore:   f.store,
		repoPusher:  f.gp,
		skillStore:  sk,
		skillPusher: pusher,
	}}
	return f, m, sk, fakes["exec-skills"]
}

// TestRepoAddRollsBackOnNamespaceCollision pins the one skill error that fails
// a registration: a repo whose skill namespace another source already holds
// can never store its skills, so the add fails AND the registration is undone.
// Every other skill error is reported with the registration kept.
func TestRepoAddRollsBackOnNamespaceCollision(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m, sk, _ := newRepoSkillsFixture(t)
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Skills: []*executorpb.RepoSkill{{Namespace: "ops_tools", Name: "demo"}},
	}
	sk.putErr = &skills.NamespaceHeldError{Namespace: "ops_tools", Holder: "repo:other_repo"}

	_, _, err := m.AddRepo(alice, "fresh_tools", "https://example.net/fresh.git", "main", 0)
	c.Require().Error(err, "AddRepo whose skills collide with another source's namespace")
	c.StrContains(err.Error(), `namespace "ops_tools" is already provided by repo:other_repo`, "AddRepo's error")

	rows, lerr := m.ListRepos(alice)
	c.Require().NoError(lerr, "ListRepos after the collision")
	for _, r := range rows {
		c.NotEq("fresh_tools", r.Name, "a registration whose skills collided survived the failed add")
	}
	c.Require().Len(f.store.deleted, 1, "deletes after the collision rollback")
	c.Eq([2]string{"u_alice", "fresh_tools"}, f.store.deleted[0], "the delete")

	// A skill error that is NOT a collision keeps the registration and is
	// reported in the response — the scripts half works, and the operator can
	// fix the skill.
	sk.putErr = nil
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Scripts:     []*executorpb.RepoScript{{Name: "rotate_keys"}},
		SkillErrors: []string{"skills/demo: symlink references/a.md is not followed"},
	}
	row, refresh, err := m.AddRepo(alice, "reported_tools", "https://example.net/reported.git", "main", 0)
	c.Require().NoError(err, "AddRepo with a per-skill error")
	c.Eq("reported_tools", row.Name, "the registration was kept")
	c.EqDeep([]string{"skills/demo: symlink references/a.md is not followed"}, refresh.SkillErrors, "the skill error reached the caller")
	c.Len(f.store.deleted, 1, "a reported (non-collision) skill error rolled the registration back")
}

// TestRepoRemoveDropsItsSkillsThenPushes pins RemoveRepo's order and effects:
// the repo's own repo:<name> skill set is deleted (the source marker is the
// only handle on it), the registration goes, the cached inventory is evicted,
// and a removal that actually dropped rows pushes the corpus on.
func TestRepoRemoveDropsItsSkillsThenPushes(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m, sk, client := newRepoSkillsFixture(t)
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	if _, _, err := m.AddRepo(alice, "ops_tools", "https://example.net/ops.git", "main", 0); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	c.Require().Eq(1, client.syncCount(), "the add's skill write pushed the corpus")
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); !ok {
		t.Fatal("precondition: ops_tools was never cached")
	}

	c.Require().NoError(m.RemoveRepo(alice, "ops_tools"), "RemoveRepo")
	c.Require().Eq(2, sk.putCount(), "PutSet calls (the add's write, then the removal's empty set)")
	c.Eq(skills.SetKey{OwnerUserID: "u_alice", Source: "repo:ops_tools"}, sk.puts[1], "the removal's set key")
	c.Len(sk.sets[sk.puts[1]], 0, "the removal's set is not empty")
	c.Eq(2, client.syncCount(), "the removal did not push the corpus on")
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok {
		t.Error("the removed source's cached inventory survived")
	}
}

// TestRepoListCountsSkillsAndScripts pins the two count columns: skills is how
// many rows the source owns for this owner (its repo:<name> set), and scripts
// is the cached discovery's count — both owner-scoped, so another owner's rows
// never inflate them.
func TestRepoListCountsSkillsAndScripts(t *testing.T) {
	c := assert.NewCollecting(t)
	f, m, sk, _ := newRepoSkillsFixture(t)
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Scripts:  []*executorpb.RepoScript{{Name: "rotate_keys"}, {Name: "deploy"}},
		Packages: []*executorpb.RepoPackage{{Name: "opslib"}},
	}
	if _, _, err := m.AddRepo(alice, "ops_tools", "https://example.net/ops.git", "main", 0); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	// The store's corpus: two rows for this repo, one for another source, and
	// one belonging to another owner entirely.
	sk.rows = []skills.Record{
		{ID: "a", Namespace: "ops_tools", Name: "demo", Source: "repo:ops_tools", OwnerUserID: "u_alice", Enabled: true},
		{ID: "b", Namespace: "ops_tools", Name: "other", Source: "repo:ops_tools", OwnerUserID: "u_alice"},
		{ID: "c", Namespace: "shared_lib", Name: "demo", Source: "repo:shared_lib", OwnerUserID: "u_alice"},
		{ID: "d", Namespace: "ops_tools", Name: "bobs", Source: "repo:ops_tools", OwnerUserID: "u_bob"},
	}

	rows, err := m.ListRepos(alice)
	c.Require().NoError(err, "ListRepos")
	var found bool
	for _, r := range rows {
		if r.Name != "ops_tools" {
			continue
		}
		found = true
		c.Eq(2, r.Skills, "ops_tools skill count")
		c.Eq(2, r.Scripts, "ops_tools script count")
		c.Eq(defaultResolveCommit, r.Commit, "ops_tools resolved commit")
		c.False(r.ResolvedAt.IsZero(), "ops_tools resolved_at was never recorded")
	}
	c.True(found, "ops_tools missing from alice's list: %+v", rows)
}

// TestRepoSetChangesOneHalfAndRearms pins SetRepo: an unnamed half is left
// alone, a named one is written, an unknown name is ErrNotFound, and the
// schedule is re-armed from the store's own state afterwards (a source with an
// interval gets a timer; clearing it removes the timer).
func TestRepoSetChangesOneHalfAndRearms(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	timers := &fakeAfter{}
	sched := newRepoScheduler(f.gp, f.store)
	sched.now = func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }
	sched.after = timers.after
	m := connectRepos{c: &Controller{repoStore: f.store, repoPusher: f.gp}, sched: sched}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// --ref only: the interval stays on demand, and nothing is armed.
	ref := "v2"
	row, err := m.SetRepo(alice, connectapi.RepoSetRequest{Name: "ops_tools", Ref: &ref})
	c.Require().NoError(err, "SetRepo ref")
	c.Eq("v2", row.Ref, "the new ref")
	c.Eq(time.Duration(0), row.RefreshInterval, "a ref-only set changed the interval")
	c.Eq(0, timers.count(), "a ref-only set armed a timer for an on-demand source")

	// --refresh only: the ref is untouched and the timer is armed, one interval
	// out from the source's last resolution.
	c.Require().NoError(f.store.SetResolved(alice, "u_alice", "ops_tools", defaultResolveCommit, sched.now()), "record a resolution")
	interval := 20 * time.Minute
	row, err = m.SetRepo(alice, connectapi.RepoSetRequest{Name: "ops_tools", RefreshInterval: &interval})
	c.Require().NoError(err, "SetRepo interval")
	c.Eq("v2", row.Ref, "an interval-only set repointed the source")
	c.Eq(20*time.Minute, row.RefreshInterval, "the new interval")
	c.Require().Eq(1, timers.count(), "an interval set armed no timer")
	c.Eq(20*time.Minute, timers.last().delay, "armed delay")

	// --refresh 0 (on demand): the timer goes away.
	off := time.Duration(0)
	_, err = m.SetRepo(alice, connectapi.RepoSetRequest{Name: "ops_tools", RefreshInterval: &off})
	c.Require().NoError(err, "SetRepo off")
	c.Eq(0, len(sched.timers), "clearing the interval left a timer armed")

	// A name that is not registered is ErrNotFound, and nothing is written.
	before := f.clients["exec-alice"].callCount()
	if _, err := m.SetRepo(alice, connectapi.RepoSetRequest{Name: "no_such_source", Ref: &ref}); err != repos.ErrNotFound {
		t.Fatalf("SetRepo(unknown) = %v, want ErrNotFound", err)
	}
	c.Eq(before, f.clients["exec-alice"].callCount(), "SetRepo refreshed; it must not")
}

// TestRepoRemoveDuringRefreshStoresNothing pins the store-write recheck: a
// refresh clones for minutes with nothing holding the registration, so a
// `repo remove` can land in the middle of one. Writing afterwards would undo
// the removal — the repo's repo:<name> skills would reappear and its commit
// would be recorded against a row that no longer exists.
func TestRepoRemoveDuringRefreshStoresNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	sk := &repoSkillStore{changed: true}
	f.gp.skillStore = sk
	client := f.clients["exec-alice"]
	client.entered = make(chan struct{})
	client.release = make(chan struct{})
	client.resp = &executorpb.SyncRepoResponse{
		Skills: []*executorpb.RepoSkill{{Namespace: "ops_tools", Name: "demo"}},
	}
	m := connectRepos{c: &Controller{repoStore: f.store, repoPusher: f.gp, skillStore: sk}}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	const url = "https://example.net/ops.git"

	done := make(chan error, 1)
	go func() {
		_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", url, "main")
		done <- err
	}()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never reached SyncRepo")
	}

	// The operator removes the repo while the refresh is in flight. The
	// removal drops the repo's skill set first, which is the only write the
	// test should ever see.
	c.Require().NoError(m.RemoveRepo(alice, "ops_tools"), "RemoveRepo during the refresh")
	c.Require().Eq(1, sk.putCount(), "PutSet calls after the removal")

	close(client.release)
	err := <-done

	c.Require().Error(err, "a refresh whose registration was removed mid-flight reported success")
	c.ErrorIs(err, repos.ErrNotFound, "the removed refresh's error")
	c.StrContains(err.Error(), "removed while the refresh was in flight", "the removed refresh's error")

	// Nothing was written: the removal's empty set is still the last one, and
	// no commit was recorded.
	c.Eq(1, sk.putCount(), "the refresh wrote skills after the removal")
	c.Len(sk.sets[sk.puts[0]], 0, "the removal's set was not the last write")
	if rec, ok := f.store.lastResolved("u_alice", "ops_tools"); ok {
		t.Errorf("the refresh recorded a commit (%q) against a removed registration", rec.Commit)
	}
	rows, lerr := f.store.List(context.Background(), "u_alice")
	c.Require().NoError(lerr, "list after the race")
	for _, r := range rows {
		c.NotEq("ops_tools", r.Name, "the removed registration came back")
	}
	// The fan-out's cached inventory is dropped with it.
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok {
		t.Error("the refresh repopulated the cache of a removed source")
	}
}

// TestRepoAddRollbackKeepsAnExistingTimer pins the rollback's schedule rule: a
// failed RE-add restores a registration that is still scheduled, so its timer
// must survive. Disarming it would leave an interval in the store and no timer
// until the daemon restarts.
func TestRepoAddRollbackKeepsAnExistingTimer(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	timers := &fakeAfter{}
	sched := newRepoScheduler(f.gp, f.store)
	sched.now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	sched.after = timers.after
	m := connectRepos{c: &Controller{repoStore: f.store, repoPusher: f.gp}, sched: sched}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	// A successful add that schedules the repo.
	if _, _, err := m.AddRepo(alice, "scheduled_tools", "https://example.net/s.git", "main", 20*time.Minute); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	c.Require().Eq(1, timers.count(), "the add armed no timer")
	c.Require().Len(sched.timers, 1, "armed timers")

	// The clone breaks, and the SAME repo is re-added. The add fails and its
	// registration is restored — with its timer still armed.
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")
	_, _, err := m.AddRepo(alice, "scheduled_tools", "https://example.net/moved.git", "v2", 5*time.Minute)
	c.Require().Error(err, "the re-add with a failing first refresh")
	c.Require().Len(sched.timers, 1, "the failed re-add disarmed a live timer")
	if _, ok := sched.timers[repoKey("u_alice", "scheduled_tools")]; !ok {
		t.Error("the failed re-add left the repo with no timer")
	}
	// ...and the store is back to the pre-add schedule, which is what that
	// timer belongs to.
	rows, lerr := f.store.List(context.Background(), "u_alice")
	c.Require().NoError(lerr, "list")
	for _, r := range rows {
		if r.Name == "scheduled_tools" {
			c.Eq(20*time.Minute, r.RefreshInterval, "the restored interval")
			c.Eq("https://example.net/s.git", r.URL, "the restored url")
		}
	}
}

// TestRepoAddRollbackLeavesNoTimerForANewRegistration is the other direction: a
// failed FIRST add leaves nothing registered, so nothing may stay armed either.
func TestRepoAddRollbackLeavesNoTimerForANewRegistration(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	timers := &fakeAfter{}
	sched := newRepoScheduler(f.gp, f.store)
	sched.now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	sched.after = timers.after
	m := connectRepos{c: &Controller{repoStore: f.store, repoPusher: f.gp}, sched: sched}
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	f.clients["exec-alice"].err = errors.New("git clone failed: repository not found")
	_, _, err := m.AddRepo(alice, "new_scheduled", "https://example.net/new.git", "main", 20*time.Minute)
	c.Require().Error(err, "the first add with a failing refresh")
	c.Len(sched.timers, 0, "the rolled-back registration left a timer armed")
	c.Eq(0, timers.count(), "a rolled-back registration armed a timer")
	rows, lerr := f.store.List(context.Background(), "u_alice")
	c.Require().NoError(lerr, "list")
	for _, r := range rows {
		c.NotEq("new_scheduled", r.Name, "the rolled-back registration survived")
	}
}
