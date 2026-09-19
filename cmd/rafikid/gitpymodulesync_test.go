// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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
	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// fakeGitSourceStore is an in-memory gitpymodules.Store. Upsert semantics
// mirror the real store: Put for an existing (owner, name) repoints url/ref
// in place rather than appending a row.
type fakeGitSourceStore struct {
	mu      sync.Mutex
	rows    map[string][]gitpymodules.GitSourceRecord
	listed  []string
	deleted [][2]string // Delete calls as {ownerUserID, name}, in call order
}

func (s *fakeGitSourceStore) Put(_ context.Context, ownerUserID, name, url, ref string) (gitpymodules.GitSourceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rows[ownerUserID] {
		if r.Name == name {
			s.rows[ownerUserID][i].URL = url
			s.rows[ownerUserID][i].Ref = ref
			return s.rows[ownerUserID][i], nil
		}
	}
	rec := gitpymodules.GitSourceRecord{
		ID:          int64(len(s.rows[ownerUserID]) + 1),
		OwnerUserID: ownerUserID,
		Name:        name,
		URL:         url,
		Ref:         ref,
	}
	s.rows[ownerUserID] = append(s.rows[ownerUserID], rec)
	return rec, nil
}

func (s *fakeGitSourceStore) List(_ context.Context, ownerUserID string) ([]gitpymodules.GitSourceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = append(s.listed, ownerUserID)
	return append([]gitpymodules.GitSourceRecord(nil), s.rows[ownerUserID]...), nil
}

func (s *fakeGitSourceStore) Delete(_ context.Context, ownerUserID, name string) error {
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
	return gitpymodules.ErrNotFound
}

// fakeGitSourceClient satisfies executorpbconnect.ExecutorServiceClient by
// embedding the interface and overriding ONLY SyncPyModuleGitSource — the
// same loud-failure-on-anything-else trick fakePymoduleClient uses. resp/err
// are read-only after construction; entered/release (both nil by default)
// let a test block a response, and requests is appended under mu because
// refresh fans out concurrently.
type fakeGitSourceClient struct {
	executorpbconnect.ExecutorServiceClient
	mu       sync.Mutex
	requests []*executorpb.SyncPyModuleGitSourceRequest
	resp     *executorpb.SyncPyModuleGitSourceResponse
	err      error
	entered  chan struct{}
	release  chan struct{}
}

func (c *fakeGitSourceClient) SyncPyModuleGitSource(ctx context.Context, req *connect.Request[executorpb.SyncPyModuleGitSourceRequest]) (*connect.Response[executorpb.SyncPyModuleGitSourceResponse], error) {
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
	if c.err != nil {
		return nil, c.err
	}
	return connect.NewResponse(c.resp), nil
}

func (c *fakeGitSourceClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// gitSourceFixture builds a pusher over three live executors — alice's, bob's
// and one whose owner username does not resolve — each answering git refresh
// through its own client, and a store holding one registered source per
// resolved owner.
type gitSourceFixture struct {
	gp      *gitPymodulePusher
	pool    *fakePymodulePool
	store   *fakeGitSourceStore
	clients map[string]*fakeGitSourceClient
}

func newGitSourceFixture() *gitSourceFixture {
	live := []execpool.LiveExecutor{
		{
			Executor: executors.Executor{ID: "exec-alice", Labels: map[string]string{"owner": "alice"}},
			Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-bob", Labels: map[string]string{"owner": "bob"}},
			Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-ghost", Labels: map[string]string{"owner": "ghost"}},
			Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
		},
	}
	store := &fakeGitSourceStore{rows: map[string][]gitpymodules.GitSourceRecord{
		"u_alice": {
			{ID: 1, OwnerUserID: "u_alice", Name: "ops_tools", URL: "https://example.net/ops.git", Ref: "main"},
			{ID: 3, OwnerUserID: "u_alice", Name: "shared_lib", URL: "https://example.net/lib.git", Ref: "v2"},
		},
		"u_bob": {
			{ID: 2, OwnerUserID: "u_bob", Name: "bob_lib", URL: "https://example.net/boblib.git", Ref: "trunk"},
		},
	}}
	clients := map[string]*fakeGitSourceClient{
		"exec-alice": {},
		"exec-bob":   {},
		"exec-ghost": {},
	}
	pool := &fakePymodulePool{live: live, clients: map[string]executorpbconnect.ExecutorServiceClient{}}
	for id, c := range clients {
		pool.clients[id] = c
	}
	gp := newGitPymodulePusher(pool, store, func(_ context.Context, username string) (string, bool) {
		id, ok := map[string]string{"alice": "u_alice", "bob": "u_bob"}[username]
		return id, ok
	})
	return &gitSourceFixture{gp: gp, pool: pool, store: store, clients: clients}
}

// gitSourceNames returns the names a SyncPyModuleGitSource request carries.
func gitSourceNames(resp *executorpb.SyncPyModuleGitSourceResponse) []string {
	out := make([]string, 0, len(resp.GetScripts())+len(resp.GetPackages()))
	for _, s := range resp.GetScripts() {
		out = append(out, "script:"+s.GetName())
	}
	for _, p := range resp.GetPackages() {
		out = append(out, "package:"+p.GetName())
	}
	return out
}

// TestGitPymodulePusherRefreshCachesLatestInventory pins the cache contract:
// the fan-out carries the registration (name, url, ref) to each executor, and
// the cache ends up holding the LAST response to arrive — the first
// responder's snapshot is visible in the cache before the second is released,
// and the second's overwrites it.
func TestGitPymodulePusherRefreshCachesLatestInventory(t *testing.T) {
	f := newGitSourceFixture()
	// Two executors both resolve to alice: a second live executor for the
	// same owner, so "latest" is a two-way race the test controls. Both block
	// until released, so the release order — first early, second late — makes
	// "the last one to respond" deterministic.
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	early := &fakeGitSourceClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	late := &fakeGitSourceClient{
		resp: &executorpb.SyncPyModuleGitSourceResponse{
			Scripts:   []*executorpb.GitSourceScript{{Name: "late_script"}},
			Packages:  []*executorpb.GitSourcePackage{{Name: "late_pkg"}},
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
	for name, c := range map[string]*fakeGitSourceClient{"exec-alice": early, "exec-alice2": late} {
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
		t.Errorf("SyncPyModuleGitSource payload = name %q url %q ref %q, want ops_tools/url/main", r.GetName(), r.GetUrl(), r.GetRef())
	}

	close(early.release)

	// Wait until the FIRST responder's snapshot is actually cached, so the
	// second release below provably overwrites it.
	deadline := time.Now().Add(waitLimit)
	for {
		if inv, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok && len(inv.Scripts) == 0 {
			break // the early client answers with an empty (nil) inventory
		}
		if time.Now().After(deadline) {
			t.Fatal("the first responder's inventory never landed in the cache")
		}
		time.Sleep(time.Millisecond)
	}

	close(late.release)
	if err := <-done; err != nil {
		t.Fatalf("refresh = %v, want nil", err)
	}

	inv, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	if !ok {
		t.Fatal("inventoryFor found nothing after a refresh that had responders")
	}
	if got, want := gitSourceNames(&executorpb.SyncPyModuleGitSourceResponse{Scripts: inv.Scripts, Packages: inv.Packages}),
		[]string{"script:late_script", "package:late_pkg"}; !slices.Equal(got, want) {
		t.Errorf("cached inventory = %v, want %v (the LAST responder's, not the first's)", got, want)
	}
	if !inv.VenvReady {
		t.Error("cached inventory lost venvReady")
	}

	// The same inventory under bob's name/owner is invisible: the cache key
	// carries the owner.
	if _, ok := f.gp.inventoryFor("u_bob", "ops_tools"); ok {
		t.Error("alice's cached inventory leaked under bob's owner")
	}
}

// TestGitPymodulePusherRefreshRunsExecutorsInParallel proves the fan-out is
// not serial — the same technique as the blob-sync
// TestPymodulePusherPushAllRunsExecutorsConcurrently: each fake RPC blocks
// until released, and the test must observe BOTH entered before releasing
// either. A sequential refresh never enters the second push.
func TestGitPymodulePusherRefreshRunsExecutorsInParallel(t *testing.T) {
	f := newGitSourceFixture()
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	early := &fakeGitSourceClient{
		resp:    &executorpb.SyncPyModuleGitSourceResponse{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	late := &fakeGitSourceClient{
		resp:    &executorpb.SyncPyModuleGitSourceResponse{},
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
	for name, c := range map[string]*fakeGitSourceClient{"exec-alice": early, "exec-alice2": late} {
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
		if err != nil {
			t.Fatalf("refresh = %v, want nil", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("refresh did not return after both executors were released")
	}
}

// TestGitPymodulePusherAllInventorySpansMultipleSources pins the discovery
// read: every cached source name for ONE owner comes back, and no other
// owner's does — the "list everything" path Task 3.2's inventories build on.
func TestGitPymodulePusherAllInventorySpansMultipleSources(t *testing.T) {
	f := newGitSourceFixture()

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err != nil {
		t.Fatalf("refresh ops_tools: %v", err)
	}
	if _, err := f.gp.refresh(context.Background(), "u_alice", "shared_lib", "https://example.net/lib.git", "v2"); err != nil {
		t.Fatalf("refresh shared_lib: %v", err)
	}
	if _, err := f.gp.refresh(context.Background(), "u_bob", "bob_lib", "https://example.net/boblib.git", "trunk"); err != nil {
		t.Fatalf("refresh bob_lib: %v", err)
	}

	alice := f.gp.allInventory("u_alice")
	if len(alice) != 2 {
		t.Fatalf("allInventory(u_alice) = %d source(s) %v, want 2", len(alice), alice)
	}
	for _, name := range []string{"ops_tools", "shared_lib"} {
		if _, ok := alice[name]; !ok {
			t.Errorf("allInventory(u_alice) is missing %q", name)
		}
	}
	if _, ok := alice["bob_lib"]; ok {
		t.Error("allInventory(u_alice) leaked bob's source")
	}

	bob := f.gp.allInventory("u_bob")
	if len(bob) != 1 {
		t.Fatalf("allInventory(u_bob) = %d source(s), want 1", len(bob))
	}
	if _, ok := bob["bob_lib"]; !ok {
		t.Error("allInventory(u_bob) is missing bob_lib")
	}

	// Every fan-out was owner-scoped: alice's executors got alice's two
	// refreshes, bob's got his one, and the unresolvable ghost got nothing.
	if got := f.clients["exec-alice"].callCount(); got != 2 {
		t.Errorf("exec-alice received %d refresh RPC(s), want 2", got)
	}
	if got := f.clients["exec-bob"].callCount(); got != 1 {
		t.Errorf("exec-bob received %d refresh RPC(s), want 1", got)
	}
	if got := f.clients["exec-ghost"].callCount(); got != 0 {
		t.Errorf("exec-ghost received %d refresh RPC(s), want 0", got)
	}
}

// Both aggregation branches of a mixed fan-out: (a) every eligible executor
// failed → the named error and an empty cache; (b) one fails, others succeed
// → nil error and the successful snapshot cached, the failure contributing
// nothing and overwriting nothing.
func TestGitPymodulePusherRefreshEveryExecutorFailingIsAnError(t *testing.T) {
	f := newGitSourceFixture()
	f.clients["exec-alice"].err = errors.New("git clone failed: authentication failed")

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err == nil {
		t.Fatal("refresh with every eligible executor failing: succeeded, want an error")
	} else if !strings.Contains(err.Error(), "every eligible executor failed (1)") {
		t.Errorf("refresh error = %v, want it to name the failed count", err)
	}
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok {
		t.Error("a fully-failed refresh left a cache entry behind")
	}
}

func TestGitPymodulePusherRefreshSurvivesOneFailingExecutor(t *testing.T) {
	f := newGitSourceFixture()
	// A second executor for the same owner: one fails, one answers.
	second := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-alice2", Labels: map[string]string{"owner": "alice"}},
		Describe: &executorpb.DescribeResponse{PymoduleGitSync: true},
	}
	f.pool.live = append(f.pool.live, second)
	ok := &fakeGitSourceClient{
		resp: &executorpb.SyncPyModuleGitSourceResponse{
			Scripts:   []*executorpb.GitSourceScript{{Name: "rotate_keys"}},
			VenvReady: true,
		},
	}
	f.pool.clients["exec-alice2"] = ok
	f.clients["exec-alice2"] = ok
	f.clients["exec-alice"].err = errors.New("git fetch failed: network unreachable")

	inv, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main")
	if err != nil {
		t.Fatalf("refresh with one responder = %v, want nil", err)
	}
	if len(inv.Scripts) != 1 || inv.Scripts[0].GetName() != "rotate_keys" || !inv.VenvReady {
		t.Errorf("refresh returned %+v, want the successful executor's snapshot", inv)
	}
	cached, present := f.gp.inventoryFor("u_alice", "ops_tools")
	if !present || len(cached.Scripts) != 1 || cached.Scripts[0].GetName() != "rotate_keys" {
		t.Errorf("cached inventory = %+v (present %v), want the successful executor's snapshot", cached, present)
	}
}

// TestGitPymodulePusherRefreshSkipsIneligibleExecutors: the Describe flag and
// the owner label are both gates. An executor without the git-sync flag, one
// without an owner label, and one whose owner does not resolve receive
// nothing — and a refresh with no eligible executor at all fails loudly
// rather than silently succeeding with an empty inventory.
func TestGitPymodulePusherRefreshSkipsIneligibleExecutors(t *testing.T) {
	f := newGitSourceFixture()
	// exec-alice loses the flag entirely: the source's own owner has no
	// eligible executor left, so the refresh must fail, not no-op.
	f.pool.live[0].Describe = &executorpb.DescribeResponse{}

	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err == nil {
		t.Fatal("refresh with no eligible executor: succeeded, want an error")
	}
	for id, c := range f.clients {
		if got := c.callCount(); got != 0 {
			t.Errorf("%s received %d refresh RPC(s); an ineligible executor must receive none", id, got)
		}
	}
	if _, ok := f.gp.inventoryFor("u_alice", "ops_tools"); ok {
		t.Error("a failed refresh left a cache entry behind")
	}

	// With the flag back on, the refresh reaches exactly the owner's own
	// resolvable executors.
	f.pool.live[0].Describe = &executorpb.DescribeResponse{PymoduleGitSync: true}
	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err != nil {
		t.Fatalf("refresh after re-enabling the flag: %v", err)
	}
	if got := f.clients["exec-alice"].callCount(); got != 1 {
		t.Errorf("exec-alice received %d refresh RPC(s), want 1", got)
	}
	if got := f.clients["exec-bob"].callCount(); got != 0 {
		t.Errorf("exec-bob received %d refresh RPC(s); alice's refresh must not reach bob's executor", got)
	}
}

// A source whose venv failed to build still caches: discovery worked, only
// the build didn't — the snapshot carries ready=false plus the executor's
// error so the CLI can print it and the inventory is not lost.
func TestGitPymodulePusherRefreshCachesVenvFailure(t *testing.T) {
	f := newGitSourceFixture()
	f.clients["exec-alice"].resp = &executorpb.SyncPyModuleGitSourceResponse{
		Scripts:   []*executorpb.GitSourceScript{{Name: "rotate_keys"}},
		VenvReady: false,
		VenvError: "uv sync failed: no solution",
	}
	if _, err := f.gp.refresh(context.Background(), "u_alice", "ops_tools", "https://example.net/ops.git", "main"); err != nil {
		t.Fatalf("refresh = %v, want nil", err)
	}
	inv, ok := f.gp.inventoryFor("u_alice", "ops_tools")
	if !ok {
		t.Fatal("inventoryFor found nothing")
	}
	if inv.VenvReady || inv.VenvError != "uv sync failed: no solution" {
		t.Errorf("cached venv state = ready %v error %q, want false + the executor's error", inv.VenvReady, inv.VenvError)
	}
}
