// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

// fakeRepoStore is an in-memory repos.Store. Upsert semantics
// mirror the real store: Put for an existing (owner, name) repoints url/ref
// in place rather than appending a row, and leaves the resolved state alone.
type fakeRepoStore struct {
	mu       sync.Mutex
	rows     map[string][]repos.Record
	listed   []string
	deleted  [][2]string // Delete calls as {ownerUserID, name}, in call order
	resolved []resolvedCall
	allCalls int
}

// resolvedCall is one SetResolved call, in call order.
type resolvedCall struct {
	OwnerUserID, Name, Commit string
	At                        time.Time
}

// lastResolved returns the SetResolved call for (owner, name), or false when
// the refresh never recorded one.
func (s *fakeRepoStore) lastResolved(ownerUserID, name string) (resolvedCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.resolved) - 1; i >= 0; i-- {
		if s.resolved[i].OwnerUserID == ownerUserID && s.resolved[i].Name == name {
			return s.resolved[i], true
		}
	}
	return resolvedCall{}, false
}

func (s *fakeRepoStore) Put(_ context.Context, ownerUserID, name, url, ref string) (repos.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rows[ownerUserID] {
		if r.Name == name {
			// A repoint changes url/ref only: the resolved state describes the
			// ref the row is leaving.
			s.rows[ownerUserID][i].URL = url
			s.rows[ownerUserID][i].Ref = ref
			return s.rows[ownerUserID][i], nil
		}
	}
	rec := repos.Record{
		ID:          int64(len(s.rows[ownerUserID]) + 1),
		OwnerUserID: ownerUserID,
		Name:        name,
		URL:         url,
		Ref:         ref,
	}
	s.rows[ownerUserID] = append(s.rows[ownerUserID], rec)
	return rec, nil
}

func (s *fakeRepoStore) List(_ context.Context, ownerUserID string) ([]repos.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = append(s.listed, ownerUserID)
	return append([]repos.Record(nil), s.rows[ownerUserID]...), nil
}

// SetResolved mirrors the real store: the commit and instant land on the row,
// and an unknown (owner, name) is ErrNotFound.
func (s *fakeRepoStore) SetResolved(_ context.Context, ownerUserID, name, commit string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, resolvedCall{OwnerUserID: ownerUserID, Name: name, Commit: commit, At: at})
	for i, r := range s.rows[ownerUserID] {
		if r.Name == name {
			s.rows[ownerUserID][i].Commit = commit
			s.rows[ownerUserID][i].ResolvedAt = at
			return nil
		}
	}
	return repos.ErrNotFound
}

// SetRefreshInterval mirrors the real store's three answers: a real interval,
// zero clearing it, and the named sentinel below the floor.
func (s *fakeRepoStore) SetRefreshInterval(_ context.Context, ownerUserID, name string, d time.Duration) error {
	if d < 0 || (d > 0 && d < repos.MinRefreshInterval) {
		return repos.ErrIntervalTooShort
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rows[ownerUserID] {
		if r.Name == name {
			s.rows[ownerUserID][i].RefreshInterval = d
			return nil
		}
	}
	return repos.ErrNotFound
}

// ListAll returns every owner's rows ordered by (owner, name), the
// unattributed bucket first — the scheduler's read.
func (s *fakeRepoStore) ListAll(_ context.Context) ([]repos.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allCalls++
	var out []repos.Record
	for _, owner := range slices.Sorted(maps.Keys(s.rows)) {
		out = append(out, s.rows[owner]...)
	}
	return out, nil
}

func (s *fakeRepoStore) Delete(_ context.Context, ownerUserID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.rows[ownerUserID]
	for i, r := range rows {
		if r.Name == name {
			s.rows[ownerUserID] = append(rows[:i:i], rows[i+1:]...)
			s.deleted = append(s.deleted, [2]string{ownerUserID, name})
			return nil
		}
	}
	return repos.ErrNotFound
}

// fakeRepoClient satisfies executorpbconnect.ExecutorServiceClient by
// embedding the interface and overriding ONLY SyncRepo and ResolveRepoRef —
// the same loud-failure-on-anything-else trick fakePymoduleClient uses.
// resp/err are read-only after construction; entered/release (both nil by
// default) let a test block a response, and requests is appended under mu
// because refresh fans out concurrently. The resolve fields are separate from
// the SyncRepo ones on purpose: a test that makes the SYNC fail must still let
// the ref resolve, and vice versa.
type fakeRepoClient struct {
	executorpbconnect.ExecutorServiceClient
	mu        sync.Mutex
	requests  []*executorpb.SyncRepoRequest
	resp      *executorpb.SyncRepoResponse
	err       error
	entered   chan struct{}
	release   chan struct{}
	onCall    func() // invoked once, before the response/err is produced
	resolves  []*executorpb.ResolveRepoRefRequest
	resolveOK *executorpb.ResolveRepoRefResponse
	resolveEr error
}

// defaultResolveCommit is what a fake executor resolves a ref to unless the
// test says otherwise.
const defaultResolveCommit = "0123456789abcdef0123456789abcdef01234567"

func (c *fakeRepoClient) ResolveRepoRef(_ context.Context, req *connect.Request[executorpb.ResolveRepoRefRequest]) (*connect.Response[executorpb.ResolveRepoRefResponse], error) {
	c.mu.Lock()
	c.resolves = append(c.resolves, req.Msg)
	c.mu.Unlock()
	if c.resolveEr != nil {
		return nil, c.resolveEr
	}
	if c.resolveOK != nil {
		return connect.NewResponse(c.resolveOK), nil
	}
	return connect.NewResponse(&executorpb.ResolveRepoRefResponse{Commit: defaultResolveCommit}), nil
}

func (c *fakeRepoClient) resolveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resolves)
}

func (c *fakeRepoClient) SyncRepo(ctx context.Context, req *connect.Request[executorpb.SyncRepoRequest]) (*connect.Response[executorpb.SyncRepoResponse], error) {
	c.mu.Lock()
	c.requests = append(c.requests, req.Msg)
	c.mu.Unlock()
	if c.entered != nil {
		close(c.entered)
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.onCall != nil {
		c.onCall()
	}
	if c.err != nil {
		return nil, c.err
	}
	return connect.NewResponse(c.resp), nil
}

func (c *fakeRepoClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// repoFixture builds a pusher over three live executors — alice's, bob's
// and one whose owner username does not resolve — each answering git refresh
// through its own client, and a store holding one registered source per
// resolved owner.
type repoFixture struct {
	gp      *repoPusher
	pool    *fakePymodulePool
	store   *fakeRepoStore
	clients map[string]*fakeRepoClient
}

func newRepoFixture() *repoFixture {
	live := []execpool.LiveExecutor{
		{
			Executor: executors.Executor{ID: "exec-alice", Labels: map[string]string{"owner": "alice"}},
			Describe: &executorpb.DescribeResponse{RepoSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-bob", Labels: map[string]string{"owner": "bob"}},
			Describe: &executorpb.DescribeResponse{RepoSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-ghost", Labels: map[string]string{"owner": "ghost"}},
			Describe: &executorpb.DescribeResponse{RepoSync: true},
		},
	}
	store := &fakeRepoStore{rows: map[string][]repos.Record{
		"u_alice": {
			{ID: 1, OwnerUserID: "u_alice", Name: "ops_tools", URL: "https://example.net/ops.git", Ref: "main"},
			{ID: 3, OwnerUserID: "u_alice", Name: "shared_lib", URL: "https://example.net/lib.git", Ref: "v2"},
		},
		"u_bob": {
			{ID: 2, OwnerUserID: "u_bob", Name: "bob_lib", URL: "https://example.net/boblib.git", Ref: "trunk"},
		},
	}}
	clients := map[string]*fakeRepoClient{
		"exec-alice": {},
		"exec-bob":   {},
		"exec-ghost": {},
	}
	pool := &fakePymodulePool{live: live, clients: map[string]executorpbconnect.ExecutorServiceClient{}}
	for id, c := range clients {
		pool.clients[id] = c
	}
	// osUser is "ghost": the fixture's ghost executor carries the daemon's OS
	// username as its owner label, so it is the anonymous-enrolled case and the
	// ONLY unresolvable label that maps to the unattributed owner.
	gp := newRepoPusher(pool, store, func(_ context.Context, username string) (string, bool, error) {
		id, ok := map[string]string{"alice": "u_alice", "bob": "u_bob"}[username]
		return id, ok, nil
	}, "ghost")
	return &repoFixture{gp: gp, pool: pool, store: store, clients: clients}
}

// repoNames returns the names a SyncRepo request carries.
func repoNames(resp *executorpb.SyncRepoResponse) []string {
	out := make([]string, 0, len(resp.GetScripts())+len(resp.GetPackages()))
	for _, s := range resp.GetScripts() {
		out = append(out, "script:"+s.GetName())
	}
	for _, p := range resp.GetPackages() {
		out = append(out, "package:"+p.GetName())
	}
	return out
}

// TestRepoPusherRefreshCachesLatestInventory pins the cache contract:
// the fan-out carries the registration (name, url, ref) to each executor, and
// the cache ends up holding the LAST response to arrive — the first
// responder's snapshot is visible in the cache before the second is released,
// and the second's overwrites it.
func TestRepoPusherRefreshCachesLatestInventory(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoFixture()
	// Two executors both resolve to alice: a second live executor for the
	// same owner, so "latest" is a two-way race the test controls. Both block
	// until released, so the release order — first early, second late — makes
	// "the last one to respond" deterministic.
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	early := &fakeRepoClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	late := &fakeRepoClient{
		resp: &executorpb.SyncRepoResponse{
			Scripts:   []*executorpb.RepoScript{{Name: "late_script"}},
			Packages:  []*executorpb.RepoPackage{{Name: "late_pkg"}},
			VenvReady: true,
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.pool.clients["exec-alice"] = early
	f.pool.clients["exec-alice2"] = late
	f.clients["exec-alice"] = early
	f.clients["exec-alice2"] = late

	done := make(chan error, 1)
	go func() {
		_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
		done <- err
	}()

	const waitLimit = 5 * time.Second
	for name, c := range map[string]*fakeRepoClient{"exec-alice": early, "exec-alice2": late} {
		select {
		case <-c.entered:
		case <-time.After(waitLimit):
			t.Fatalf("%s never entered its refresh", name)
		}
	}

	// The RPC carried the registration verbatim.
	if got := early.requests; len(got) != 1 {
		t.Fatalf("exec-alice received %d request(s), want 1", len(got))
	} else if r := got[0]; r.GetName() != "ops_tools" || r.GetUrl() != "https://example.net/ops.git" || r.GetRef() != "main" {
		t.Errorf("SyncRepo payload = name %q url %q ref %q, want ops_tools/url/main", r.GetName(), r.GetUrl(), r.GetRef())
	}

	close(early.release)

	// Wait until the FIRST responder's snapshot is actually cached, so the
	// second release below provably overwrites it.
	deadline := time.Now().Add(waitLimit)
	for {
		if inv, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok && len(inv.Scripts) == 0 {
			break // the early client answers with an empty (nil) inventory
		}
		ck.Require().False(time.Now().After(deadline), "the first responder's inventory never landed in the cache")
		time.Sleep(time.Millisecond)
	}

	close(late.release)
	ck.Require().NoError(<-done, "refresh")

	inv, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	ck.Require().True(ok, "inventoryFor found nothing after a refresh that had responders")
	got, want := repoNames(&executorpb.SyncRepoResponse{Scripts: inv.Scripts, Packages: inv.Packages}),
		[]string{"script:late_script", "package:late_pkg"}
	ck.EqDiff(want, got, "cached inventory")
	ck.True(inv.VenvReady, "cached inventory lost venvReady")

	// The same inventory under bob's name/owner is invisible: the cache key
	// carries the owner.
	if _, ok := f.gp.inventoryFor("u_bob", "ops_tools"); ok {
		t.Error("alice's cached inventory leaked under bob's owner")
	}
}

// TestRepoPusherRefreshRunsExecutorsInParallel proves the fan-out is
// not serial — the same technique as the blob-sync
// TestPymodulePusherPushAllRunsExecutorsConcurrently: each fake RPC blocks
// until released, and the test must observe BOTH entered before releasing
// either. A sequential refresh never enters the second push.
func TestRepoPusherRefreshRunsExecutorsInParallel(t *testing.T) {
	f := newRepoFixture()
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	early := &fakeRepoClient{
		resp:    &executorpb.SyncRepoResponse{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	late := &fakeRepoClient{
		resp:    &executorpb.SyncRepoResponse{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.pool.clients["exec-alice"] = early
	f.pool.clients["exec-alice2"] = late

	done := make(chan error, 1)
	go func() {
		_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
		done <- err
	}()

	const waitLimit = 5 * time.Second
	for name, c := range map[string]*fakeRepoClient{"exec-alice": early, "exec-alice2": late} {
		select {
		case <-c.entered:
		case <-time.After(waitLimit):
			t.Fatalf("%s's refresh never started while the other was in flight; the fan-out is running executors sequentially", name)
		}
	}

	close(early.release)
	close(late.release)
	select {
	case err := <-done:
		assert.NewAborting(t).NoError(err, "refresh")
	case <-time.After(waitLimit):
		t.Fatal("refresh did not return after both executors were released")
	}
}

// TestRepoPusherAllInventorySpansMultipleSources pins the discovery
// read: every cached source name for ONE owner comes back, and no other
// owner's does — the "list everything" path Task 3.2's inventories build on.
func TestRepoPusherAllInventorySpansMultipleSources(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err != nil {
		t.Fatalf("refresh ops_tools: %v", err)
	}
	if _, err := f.gp.refresh(context.Background(), "u_alice", "shared_lib", "https://example.net/lib.git", "v2"); err != nil {
		t.Fatalf("refresh shared_lib: %v", err)
	}
	_, err := f.gp.refresh(context.Background(), "u_bob", "bob_lib", "https://example.net/boblib.git", "trunk")
	c.Require().NoError(err, "refresh bob_lib")

	alice := f.gp.allInventory("u_alice")
	c.Require().Len(alice, 2, "allInventory(u_alice) = %d source(s) %v, want 2", len(alice), alice)
	for _, name := range []string{"ops_tools", "shared_lib"} {
		_, ok := alice[name]
		c.True(ok, "allInventory(u_alice) is missing %q", name)
	}
	if _, ok := alice["bob_lib"]; ok {
		t.Error("allInventory(u_alice) leaked bob's source")
	}

	bob := f.gp.allInventory("u_bob")
	c.Require().Len(bob, 1, "allInventory(u_bob) = %d source(s), want 1", len(bob))
	_, ok := bob["bob_lib"]
	c.True(ok, "allInventory(u_bob) is missing bob_lib")

	// Every fan-out was owner-scoped: alice's executors got alice's two
	// refreshes, bob's got his one, and the unresolvable ghost got nothing.
	c.Eq(2, f.clients["exec-alice"].callCount(), "exec-alice received")
	c.Eq(1, f.clients["exec-bob"].callCount(), "exec-bob received")
	c.Eq(0, f.clients["exec-ghost"].callCount(), "exec-ghost received")
}

// Both aggregation branches of a mixed fan-out: (a) every eligible executor
// failed → the named error and an empty cache; (b) one fails, others succeed
// → nil error and the successful snapshot cached, the failure contributing
// nothing and overwriting nothing.
func TestRepoPusherRefreshEveryExecutorFailingIsAnError(t *testing.T) {
	f := newRepoFixture()
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err == nil {
		t.Fatal("refresh with every eligible executor failing: succeeded, want an error")
	} else if !strings.Contains(err.Error(), "every eligible executor failed (1)") {
		t.Errorf("refresh error = %v, want it to name the failed count", err)
	}
	_, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	assert.NewCollecting(t).False(ok, "a fully-failed refresh left a cache entry behind")
}

func TestRepoPusherRefreshSurvivesOneFailingExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	// A second executor for the same owner: one fails, one answers.
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	ok := &fakeRepoClient{
		resp: &executorpb.SyncRepoResponse{
			Scripts:   []*executorpb.RepoScript{{Name: "rotate_keys"}},
			VenvReady: true,
		},
	}
	f.pool.clients["exec-alice2"] = ok
	f.clients["exec-alice2"] = ok
	f.clients["exec-alice"].err = errors.New("git fetch failed: network unreachable")

	inv, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh with one responder")
	c.False(len(inv.Scripts) != 1 || inv.Scripts[0].GetName() != "rotate_keys" || !inv.VenvReady, "refresh returned %+v, want the successful executor's snapshot", inv)
	cached, present := f.gp.inventoryFor("u_alice", "ops_tools")
	c.False(!present || len(cached.Scripts) != 1 || cached.Scripts[0].GetName() != "rotate_keys", "cached inventory = %+v (present %v), want the successful executor's snapshot", cached, present)
}

// TestRepoPusherRefreshFailureKeepsPriorSnapshot pins what a sandbox
// with network none relies on: its clone fails on every refresh, and that
// failure must leave the snapshot a healthy executor already reported intact.
func TestRepoPusherRefreshFailureKeepsPriorSnapshot(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Scripts:   []*executorpb.RepoScript{{Name: "rotate_keys"}},
		VenvReady: true,
	}
	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "first refresh")

	f.clients["exec-alice"].err = errors.New("git fetch origin main: could not resolve host")
	_, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Error(err, "a refresh where every executor fails reports it")

	cached, present := f.gp.inventoryFor("u_alice", "ops_tools")
	c.False(!present || len(cached.Scripts) != 1 || cached.Scripts[0].GetName() != "rotate_keys",
		"cached inventory = %+v (present %v), want the earlier successful snapshot", cached, present)
}

// TestRepoPusherRefreshSkipsIneligibleExecutors: the Describe flag and
// the owner label are both gates. An executor without the repo-sync flag and
// one whose owner label names no active user receive nothing for a USER's
// refresh — the latter is unattributed and serves only owner ""'s sources
// (see TestRepoRefreshUnattributedExecutor) — and a refresh with no
// eligible executor at all fails loudly rather than silently succeeding with
// an empty inventory.
func TestRepoPusherRefreshSkipsIneligibleExecutors(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoFixture()
	// exec-alice loses the flag entirely: the source's own owner has no
	// eligible executor left, so the refresh must fail, not no-op.
	f.pool.live[0].Describe = &executorpb.DescribeResponse{}

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err == nil {
		t.Fatal("refresh with no eligible executor: succeeded, want an error")
	}
	for id, c := range f.clients {
		got := c.callCount()
		ck.Eq(0, got, "%s received %d refresh RPC(s); an ineligible executor must receive none", id, got)
	}
	_, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	ck.False(ok, "a failed refresh left a cache entry behind")

	// With the flag back on, the refresh reaches exactly the owner's own
	// resolvable executors.
	f.pool.live[0].Describe = &executorpb.DescribeResponse{RepoSync: true}
	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	ck.Require().NoError(err, "refresh after re-enabling the flag")
	ck.Eq(1, f.clients["exec-alice"].callCount(), "exec-alice received")
	ck.Eq(0, f.clients["exec-bob"].callCount(), "exec-bob received")
}

// A source whose venv failed to build still caches: discovery worked, only
// the build didn't — the snapshot carries ready=false plus the executor's
// error so the CLI can print it and the inventory is not lost.
func TestRepoPusherRefreshCachesVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Scripts:   []*executorpb.RepoScript{{Name: "rotate_keys"}},
		VenvReady: false,
		VenvError: "uv sync failed: no solution",
	}
	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh")
	inv, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	c.Require().True(ok, "inventoryFor found nothing")
	c.False(inv.VenvReady || inv.VenvError != "uv sync failed: no solution", "cached venv state = ready %v error %q, want false + the executor's error", inv.VenvReady, inv.VenvError)
}

// TestRepoRefreshUnattributedExecutor pins the unattributed rule: a
// live executor whose owner label resolves to NO user (the fixture's ghost)
// is the UNATTRIBUTED owner "" — the same rule the anonymous unix-socket
// caller follows — so an anonymous registration (owner "") syncs to it, while
// a user's refresh still never reaches it. A resolver ERROR is not a miss:
// the executor is skipped, so an outage never hands anonymous sources to an
// executor whose ownership the store could not settle.
func TestRepoRefreshUnattributedExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()

	// The anonymous refresh reaches ONLY the unresolvable ghost, and the RPC
	// carries the registration verbatim.
	inv, err := f.gp.refresh(context.Background(), "", "anon_repo", "https://example.net/anon.git", "main")
	c.Require().NoError(err, "refresh for the unattributed owner")
	c.Eq(1, f.clients["exec-ghost"].callCount(), "exec-ghost received")
	c.Eq(0, f.clients["exec-alice"].callCount(), "exec-alice received")
	c.Eq(0, f.clients["exec-bob"].callCount(), "exec-bob received")
	c.Require().Len(f.clients["exec-ghost"].requests, 1, "exec-ghost's requests")
	req := f.clients["exec-ghost"].requests[0]
	c.False(req.GetName() != "anon_repo" || req.GetUrl() != "https://example.net/anon.git" || req.GetRef() != "main",
		"the anonymous refresh payload = name %q url %q ref %q", req.GetName(), req.GetUrl(), req.GetRef())
	c.Len(inv.Scripts, 0, "the returned inventory's scripts")
	c.Len(inv.Packages, 0, "the returned inventory's packages")

	// The snapshot is cached under the unattributed owner, and only there.
	if _, ok := f.gp.inventoryFor("", "anon_repo"); !ok {
		t.Error("the anonymous refresh left no cache entry under the unattributed owner")
	}
	if _, ok := f.gp.inventoryFor("u_alice", "anon_repo"); ok {
		t.Error("the anonymous refresh's inventory leaked under alice's owner")
	}

	// A user's refresh still never reaches the unattributed executor.
	_, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh for alice")
	c.Eq(1, f.clients["exec-ghost"].callCount(), "exec-ghost received after alice's refresh")
	c.Eq(1, f.clients["exec-alice"].callCount(), "exec-alice received after alice's refresh")

	// A resolver ERROR (store outage) skips the executor instead of marking it
	// unattributed: the anonymous refresh fails, and the executor got nothing.
	f.gp.resolveOwnerID = func(_ context.Context, username string) (string, bool, error) {
		if username == "ghost" {
			return "", false, errors.New("users store unavailable")
		}
		id, ok := map[string]string{"alice": "u_alice", "bob": "u_bob"}[username]
		return id, ok, nil
	}
	_, err = f.gp.refresh(context.Background(), "", "anon_repo", "https://example.net/anon.git", "main")
	c.Require().Error(err, "refresh with an erroring owner lookup: succeeded, want no eligible executor")
	c.StrContains(err.Error(), `no eligible executor for owner "(unattributed)"'s repos`, "refresh error")
	c.Eq(1, f.clients["exec-ghost"].callCount(), "exec-ghost received after the resolver outage")

	// A label naming a NON-OS, unresolvable user is skipped, never treated as
	// unattributed: a removed user's executor (whose session was never
	// evicted) must not become the anonymous bucket and receive sources that
	// may embed credentials. A fifth executor whose label resolves to nothing
	// and is not the OS user gets nothing, and the anonymous refresh fails
	// because the only unattributed executor (ghost) is currently erroring.
	f.pool.live = append(f.pool.live, execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-tombstone", Labels: map[string]string{"owner": "removed_user"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	})
	f.clients["exec-tombstone"] = &fakeRepoClient{}
	f.pool.clients["exec-tombstone"] = f.clients["exec-tombstone"]
	_, err = f.gp.refresh(context.Background(), "", "anon_repo", "https://example.net/anon.git", "main")
	c.Require().Error(err, "anonymous refresh with only a non-OS unresolvable executor: succeeded, want no eligible executor")
	c.Eq(0, f.clients["exec-tombstone"].callCount(), "exec-tombstone received")
}

// TestRepoRefreshNoEligibleNamesExecutors pins the no-eligible error's
// content: it names the owner the refresh was for (unattributed included) and
// lists EVERY live executor with its id prefix, owner label and repo-sync
// flag, so an operator can see why their executor was passed over.
func TestRepoRefreshNoEligibleNamesExecutors(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	// A fourth live executor with a long id: the id renders as a prefix.
	f.pool.live = append(f.pool.live, execpool.LiveExecutor{
		Executor: executors.Executor{ID: "0123456789abcdef", Labels: map[string]string{"owner": "longowner"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	})

	// Alice's executor loses the flag: nothing is eligible for her source, and
	// the error must name the pool it saw — every live executor, flagged.
	f.pool.live[0].Describe = &executorpb.DescribeResponse{}
	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().Error(err, "refresh with no eligible executor")
	c.StrContains(err.Error(), `no eligible executor for owner "u_alice"'s repos: 4 live executor(s):`, "refresh error")
	c.StrContains(err.Error(), `exec-alice owner="alice" repo-sync=false`, "refresh error")
	c.StrContains(err.Error(), `exec-bob owner="bob" repo-sync=true`, "refresh error")
	c.StrContains(err.Error(), `exec-ghost owner="ghost" repo-sync=true`, "refresh error")
	c.StrContains(err.Error(), `0123456789ab owner="longowner" repo-sync=true`, "refresh error")
	c.NotStrContains(err.Error(), "0123456789abcdef", "the long id was not truncated to a prefix")

	// The unattributed owner is named rather than printed as an empty id. With
	// every flag off nothing is eligible even for owner "".
	for i := range f.pool.live {
		f.pool.live[i].Describe = &executorpb.DescribeResponse{}
	}
	_, err = f.gp.refresh(context.Background(), "", "anon_repo", "https://example.net/anon.git", "main")
	c.Require().Error(err, "unattributed refresh with no eligible executor")
	c.StrContains(err.Error(), `no eligible executor for owner "(unattributed)"'s repos: 4 live executor(s):`, "refresh error")

	// No executor received anything, and no cache entry was left behind.
	for id, cl := range f.clients {
		c.Eq(0, cl.callCount(), "%s received", id)
	}
	_, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	c.False(ok, "a failed refresh left a cache entry behind")
	_, ok = f.gp.inventoryFor("", "anon_repo")
	c.False(ok, "a failed refresh left a cache entry behind")
}

// TestRepoPusherResolvesOnceAndFansOutSHA pins the one-git-call contract: the
// ref is resolved ONCE, through the first eligible executor in ID order, and
// every SyncRepo in the fan-out carries that same commit. Two executors can
// therefore never land different commits for one refresh.
func TestRepoPusherResolvesOnceAndFansOutSHA(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	// A second executor for alice, so "once" is provable across a real fan-out.
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	f.clients["exec-alice2"] = &fakeRepoClient{}
	f.pool.clients["exec-alice2"] = f.clients["exec-alice2"]

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	for _, id := range []string{"exec-alice", "exec-alice2"} {
		f.clients[id].resolveOK = &executorpb.ResolveRepoRefResponse{Commit: sha}
	}

	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh")

	// Exactly one resolve, and it went to the lowest-id eligible executor.
	c.Eq(1, f.clients["exec-alice"].resolveCount(), "exec-alice resolve calls")
	c.Eq(0, f.clients["exec-alice2"].resolveCount(), "exec-alice2 resolve calls")
	c.Eq(0, f.clients["exec-bob"].resolveCount(), "exec-bob resolve calls")
	got := f.clients["exec-alice"].resolves[0]
	c.False(got.GetName() != "ops_tools" || got.GetUrl() != "https://example.net/ops.git" || got.GetRef() != "main",
		"resolve payload = name %q url %q ref %q", got.GetName(), got.GetUrl(), got.GetRef())

	// Both fan-out calls carry the resolved SHA.
	for _, id := range []string{"exec-alice", "exec-alice2"} {
		reqs := f.clients[id].requests
		c.Require().Len(reqs, 1, "%s SyncRepo requests", id)
		c.Eq(sha, reqs[0].GetCommit(), "%s SyncRepo commit", id)
	}
}

// A resolve that fails on the first executor is retried on the next, and a
// resolve that fails everywhere fails the refresh naming the source.
func TestRepoPusherResolveFallsBackAndReportsFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{RepoSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	f.clients["exec-alice2"] = &fakeRepoClient{}
	f.pool.clients["exec-alice2"] = f.clients["exec-alice2"]

	// exec-alice cannot resolve (an RPC failure), exec-alice2 can.
	f.clients["exec-alice"].resolveEr = errors.New("executor unreachable")
	_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh with a fallback executor")
	c.Eq(1, f.clients["exec-alice"].resolveCount(), "exec-alice resolve calls")
	c.Eq(1, f.clients["exec-alice2"].resolveCount(), "exec-alice2 resolve calls")
	c.Eq(defaultResolveCommit, f.clients["exec-alice2"].requests[0].GetCommit(), "the fallback's SHA fanned out")

	// A resolve that fails everywhere is a refresh failure naming the source,
	// and nothing is synced.
	f.clients["exec-alice2"].resolveOK = &executorpb.ResolveRepoRefResponse{Error: "fatal: couldn't find remote ref main"}
	f.clients["exec-alice"].requests = nil
	f.clients["exec-alice2"].requests = nil
	_, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().Error(err, "refresh with every resolve failing")
	c.StrContains(err.Error(), "resolve ops_tools", "refresh error")
	c.StrContains(err.Error(), "couldn't find remote ref main", "refresh error names the last failure")
	for _, id := range []string{"exec-alice", "exec-alice2"} {
		c.Eq(0, f.clients[id].callCount(), "%s SyncRepo calls after a failed resolve", id)
	}
}

// ─── coalescing, the skills half and the recorded commit ─────────────────────

// waiters returns how many callers are currently blocked on (owner, name)'s
// in-flight refresh — the observable that makes "the second caller joined the
// first's refresh" assertable instead of a sleep.
func (gp *repoPusher) waiters(ownerUserID, name string) int {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	call, ok := gp.inflight[repoKey(ownerUserID, name)]
	if !ok {
		return 0
	}
	return call.waiters
}

// repoSkillStore is an in-memory skills.Store for the refresh's skills half.
// PutSet records every call — the key and the records — so a test can assert
// the refresh wrote exactly its own repo:<name> set and nothing else.
type repoSkillStore struct {
	mu      sync.Mutex
	puts    []skills.SetKey
	sets    map[skills.SetKey][]skills.Record
	changed bool
	putErr  error
	// rows is what List answers — the corpus a push reads.
	rows []skills.Record
}

func (s *repoSkillStore) List(context.Context, bool) ([]skills.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]skills.Record(nil), s.rows...), nil
}
func (s *repoSkillStore) Get(context.Context, string) (skills.Record, error) {
	return skills.Record{}, skills.ErrNotFound
}
func (s *repoSkillStore) Files(context.Context, []string) (map[string][]skills.File, error) {
	return nil, nil
}
func (s *repoSkillStore) SetEnabled(context.Context, string, bool) error { return nil }
func (s *repoSkillStore) Delete(context.Context, string) error           { return nil }

func (s *repoSkillStore) PutSet(_ context.Context, key skills.SetKey, want []skills.Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, key)
	if s.putErr != nil {
		return false, s.putErr
	}
	if s.sets == nil {
		s.sets = make(map[skills.SetKey][]skills.Record)
	}
	s.sets[key] = want
	return s.changed, nil
}

func (s *repoSkillStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.puts)
}

// TestRepoRefreshCoalesces pins the one-refresh-per-source rule: a second
// caller arriving while a refresh is in flight joins it instead of resolving
// the ref and cloning again, so two concurrent callers cost exactly one
// ResolveRepoRef and one SyncRepo per executor — and both get the answer.
func TestRepoRefreshCoalesces(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	client := f.clients["exec-alice"]
	client.entered = make(chan struct{})
	client.release = make(chan struct{})

	const url = "https://example.net/ops.git"
	first := make(chan RefreshResult, 1)
	go func() {
		res, _ := f.gp.refresh(context.Background(), "u_alice", "ops_tools", url, "main")
		first <- res
	}()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first refresh never reached SyncRepo")
	}

	second := make(chan RefreshResult, 1)
	go func() {
		res, _ := f.gp.refresh(context.Background(), "u_alice", "ops_tools", url, "main")
		second <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.gp.waiters("u_alice", "ops_tools") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never joined the in-flight refresh")
		}
		time.Sleep(time.Millisecond)
	}

	close(client.release)
	var results []RefreshResult
	for i, ch := range []chan RefreshResult{first, second} {
		select {
		case res := <-ch:
			results = append(results, res)
		case <-time.After(5 * time.Second):
			t.Fatalf("caller %d never returned", i)
		}
	}

	c.Eq(1, client.resolveCount(), "ResolveRepoRef calls for two concurrent refreshes")
	c.Eq(1, client.callCount(), "SyncRepo calls for two concurrent refreshes")
	c.Eq(defaultResolveCommit, results[0].Commit, "first caller's commit")
	c.Eq(defaultResolveCommit, results[1].Commit, "the waiter got a different answer than the caller it joined")
}

// TestRepoRefreshWaiterHonoursCtx pins the waiter's own lifetime: a caller
// that gives up returns its context's error promptly and does NOT disturb the
// refresh it was waiting on, which still completes and still records its
// commit.
func TestRepoRefreshWaiterHonoursCtx(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	client := f.clients["exec-alice"]
	client.entered = make(chan struct{})
	client.release = make(chan struct{})

	const url = "https://example.net/ops.git"
	firstDone := make(chan error, 1)
	go func() {
		_, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", url, "main")
		firstDone <- err
	}()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first refresh never reached SyncRepo")
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := f.gp.refresh(ctx, "u_alice", "ops_tools", url, "main")
		waiterDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.gp.waiters("u_alice", "ops_tools") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never joined the in-flight refresh")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-waiterDone:
		c.ErrorIs(err, context.Canceled, "the waiter's error")
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter ignored its canceled context and blocked on the in-flight refresh")
	}

	// The refresh it walked away from is unaffected: it finishes and records
	// its commit.
	close(client.release)
	c.Require().NoError(<-firstDone, "the joined refresh")
	got, ok := f.store.lastResolved("u_alice", "ops_tools")
	c.Require().True(ok, "the refresh that outlived its waiter recorded no commit")
	c.Eq(defaultResolveCommit, got.Commit, "recorded commit")
}

// TestRepoRefreshStoresDiscoveredSkills pins the skills half of a refresh: the
// first successful response's discovered skills become the repo's own
// repo:<name> set (with their files), the resolved commit is recorded, and a
// write that CHANGED something pushes the corpus on.
func TestRepoRefreshStoresDiscoveredSkills(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	sk := &repoSkillStore{changed: true}
	f.gp.skillStore = sk
	var pushed int
	f.gp.skillPush = func(context.Context) { pushed++ }
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Skills: []*executorpb.RepoSkill{{
			Namespace:   "ops_tools",
			Name:        "demo",
			Description: "a demo skill",
			Body:        "# demo",
			From:        "skills/demo",
			Files: []*executorpb.SkillFile{
				{Path: "references/a.md", Mode: 0o644, Content: []byte("ref")},
			},
		}},
	}

	res, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "refresh")
	c.Eq(1, res.Skills, "refresh result skills")
	c.Empty(res.SkillErrors, "refresh result skill errors")
	c.False(res.NamespaceCollision(), "refresh result reported a namespace collision")

	c.Require().Eq(1, sk.putCount(), "PutSet calls")
	c.Eq(skills.SetKey{OwnerUserID: "u_alice", Source: "repo:ops_tools"}, sk.puts[0], "the set key")
	got := sk.sets[sk.puts[0]]
	c.Require().Len(got, 1, "stored skills")
	c.False(got[0].Namespace != "ops_tools" || got[0].Name != "demo" || got[0].Description != "a demo skill" || got[0].Body != "# demo",
		"stored skill = %+v", got[0])
	c.False(got[0].Source != "repo:ops_tools" || got[0].OwnerUserID != "u_alice", "stored skill's ownership = %+v", got[0])
	c.Require().Len(got[0].Files, 1, "stored skill's files")
	c.Eq("references/a.md", got[0].Files[0].Path, "stored file path")
	c.Eq(uint32(0o644), got[0].Files[0].Mode, "stored file mode")
	c.EqDeep([]byte("ref"), got[0].Files[0].Content, "stored file content")

	rec, ok := f.store.lastResolved("u_alice", "ops_tools")
	c.Require().True(ok, "the refresh recorded no commit")
	c.Eq(defaultResolveCommit, rec.Commit, "recorded commit")
	c.True(rec.At.After(time.Time{}), "recorded resolved_at")
	c.Eq(1, pushed, "skill pushes after a changed set")

	// A set identical to the stored one writes nothing and pushes nothing.
	sk.changed = false
	_, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "second refresh")
	c.Eq(1, pushed, "skill pushes after an unchanged set")
}

// TestRepoRefreshSkillErrorsSkipTheWrite pins the executor's per-skill errors:
// they are the refresh result's errors, nothing is written (a partial
// discovery must not replace the stored set), and the refresh itself succeeds.
func TestRepoRefreshSkillErrorsSkipTheWrite(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	sk := &repoSkillStore{changed: true}
	f.gp.skillStore = sk
	var pushed int
	f.gp.skillPush = func(context.Context) { pushed++ }
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Scripts:     []*executorpb.RepoScript{{Name: "rotate_keys"}},
		Skills:      []*executorpb.RepoSkill{{Namespace: "ops_tools", Name: "demo"}},
		SkillErrors: []string{"skills/demo: symlink references/a.md is not followed"},
	}

	res, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "a skill error must not fail the refresh")
	c.EqDeep([]string{"skills/demo: symlink references/a.md is not followed"}, res.SkillErrors, "refresh result skill errors")
	c.Eq(0, res.Skills, "skills reported as stored")
	c.Eq(0, sk.putCount(), "PutSet calls for a refresh whose discovery reported errors")
	c.Eq(0, pushed, "skill pushes")
	c.Len(res.Scripts, 1, "the scripts half still refreshed")
}

// TestRepoRefreshNamespaceCollisionIsASkillError pins the one skill error that
// is more than a report: a namespace another source already holds. It reaches
// the caller as a skill error AND as the collision flag, which is what makes
// `repo add` roll the registration back.
func TestRepoRefreshNamespaceCollisionIsASkillError(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()
	held := &skills.NamespaceHeldError{Namespace: "ops_tools", Holder: "repo:other_repo"}
	f.gp.skillStore = &repoSkillStore{putErr: held}
	f.clients["exec-alice"].resp = &executorpb.SyncRepoResponse{
		Skills: []*executorpb.RepoSkill{{Namespace: "ops_tools", Name: "demo"}},
	}

	res, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "a namespace collision is a skill error, not the refresh's error")
	c.EqDeep([]string{held.Error()}, res.SkillErrors, "refresh result skill errors")
	c.True(res.NamespaceCollision(), "the namespace collision was not flagged for the caller")
	c.Eq(0, res.Skills, "skills reported as stored after a refused write")

	// The commit is still recorded: the scripts half succeeded, and the next
	// refresh must be able to tell whether the ref moved.
	rec, ok := f.store.lastResolved("u_alice", "ops_tools")
	c.Require().True(ok, "a skill failure suppressed the recorded commit")
	c.Eq(defaultResolveCommit, rec.Commit, "recorded commit")
}

// TestRepoRefreshReportsUnchangedCommit pins the flag `repo refresh` prints as
// "(unchanged)": a refresh whose resolved commit equals the stored one did not
// move the source, and one whose commit differs did.
func TestRepoRefreshReportsUnchangedCommit(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newRepoFixture()

	res, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "first refresh")
	c.False(res.Unchanged, "the first refresh of a never-resolved source reported unchanged")

	// The same commit again: the stored commit now equals what the executor
	// resolved, so nothing moved.
	res, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "second refresh")
	c.True(res.Unchanged, "a refresh resolving the stored commit did not report unchanged")

	// A moved ref resolves a different commit, so it is a change.
	const moved = "fedcbafedcbafedcbafedcbafedcbafedcbafedc"
	f.clients["exec-alice"].resolveOK = &executorpb.ResolveRepoRefResponse{Commit: moved}
	res, err = f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	c.Require().NoError(err, "third refresh")
	c.False(res.Unchanged, "a refresh resolving a new commit reported unchanged")
	c.Eq(moved, res.Commit, "refresh result commit")
}
