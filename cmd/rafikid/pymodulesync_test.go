// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// fakePymoduleStore is an in-memory pymodules.Store. It records the owner id
// of EVERY List call, in order, so a test can assert the pusher asked for one
// owner's rows and nobody else's.
type fakePymoduleStore struct {
	rows    map[string][]pymodules.Record
	listed  []string
	deleted [][2]string // Delete calls as {ownerUserID, name}, in call order
}

func (s *fakePymoduleStore) Put(_ context.Context, ownerUserID, name, code, description string) (pymodules.Record, error) {
	rec := pymodules.Record{
		ID:          int64(len(s.rows[ownerUserID]) + 1),
		OwnerUserID: ownerUserID,
		Name:        name,
		Code:        code,
		Description: description,
	}
	s.rows[ownerUserID] = append(s.rows[ownerUserID], rec)
	return rec, nil
}

func (s *fakePymoduleStore) List(_ context.Context, ownerUserID string) ([]pymodules.Record, error) {
	s.listed = append(s.listed, ownerUserID)
	return append([]pymodules.Record(nil), s.rows[ownerUserID]...), nil
}

// Get follows the real store's latest-live-row rule: the highest-ID row still
// in the slice for (ownerUserID, name). A Delete removes the row from the
// slice, so a Get after a delete reports ErrNotFound, and a later Put
// restores it.
func (s *fakePymoduleStore) Get(_ context.Context, ownerUserID, name string) (pymodules.Record, error) {
	var best pymodules.Record
	for _, r := range s.rows[ownerUserID] {
		if r.Name == name && r.ID > best.ID {
			best = r
		}
	}
	if best.ID == 0 {
		return pymodules.Record{}, pymodules.ErrNotFound
	}
	return best, nil
}

// fakePymodulePool stands in for *execpool.Pool — the same reason
// executorPool (executor_select.go) is an interface: the pusher is testable
// without a listener, a database, or a dialling executor.
type fakePymodulePool struct {
	live   []execpool.LiveExecutor
	client *fakePymoduleClient
}

func (f *fakePymodulePool) Live() []execpool.LiveExecutor { return f.live }

func (f *fakePymodulePool) ConnectClientFor(string) (executorpbconnect.ExecutorServiceClient, error) {
	return f.client, nil
}

// fakePymoduleClient satisfies executorpbconnect.ExecutorServiceClient by
// embedding the interface and overriding ONLY SyncPyModules. Any other method
// dereferences the nil embedded interface and panics — a loud failure, which
// is what an RPC the pusher should not have made deserves.
type fakePymoduleClient struct {
	executorpbconnect.ExecutorServiceClient
	requests []*executorpb.SyncPyModulesRequest
}

func (c *fakePymoduleClient) SyncPyModules(_ context.Context, req *connect.Request[executorpb.SyncPyModulesRequest]) (*connect.Response[executorpb.SyncPyModulesResponse], error) {
	c.requests = append(c.requests, req.Msg)
	return connect.NewResponse(&executorpb.SyncPyModulesResponse{Written: int32(len(req.Msg.GetModules()))}), nil
}

// moduleNames returns the names a SyncPyModules request carries, in order.
func moduleNames(req *executorpb.SyncPyModulesRequest) []string {
	out := make([]string, 0, len(req.GetModules()))
	for _, m := range req.GetModules() {
		out = append(out, m.GetName())
	}
	return out
}

type pymoduleFixture struct {
	pp     *pymodulePusher
	pool   *fakePymodulePool
	store  *fakePymoduleStore
	client *fakePymoduleClient
}

// newPymoduleFixture builds a pusher over three live executors: one owned by
// alice, one by bob, and one whose owner username does not resolve. Each
// owner's rows are visible to the store under their own id only.
func newPymoduleFixture() *pymoduleFixture {
	live := []execpool.LiveExecutor{
		{
			Executor: executors.Executor{ID: "exec-alice", Labels: map[string]string{"owner": "alice"}},
			Describe: &executorpb.DescribeResponse{PymodulesSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-bob", Labels: map[string]string{"owner": "bob"}},
			Describe: &executorpb.DescribeResponse{PymodulesSync: true},
		},
		{
			Executor: executors.Executor{ID: "exec-ghost", Labels: map[string]string{"owner": "ghost"}},
			Describe: &executorpb.DescribeResponse{PymodulesSync: true},
		},
	}
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{
		"u_alice": {{ID: 1, OwnerUserID: "u_alice", Name: "alice_chart", Code: "def alice_chart(): pass"}},
		"u_bob":   {{ID: 2, OwnerUserID: "u_bob", Name: "bob_util", Code: "def bob_util(): pass"}},
	}}
	client := &fakePymoduleClient{}
	pool := &fakePymodulePool{live: live, client: client}
	userIDs := map[string]string{"alice": "u_alice", "bob": "u_bob"}
	pp := &pymodulePusher{
		pool:    pool,
		store:   store,
		version: "v1",
		resolveOwnerID: func(_ context.Context, username string) (string, bool) {
			id, ok := userIDs[username]
			return id, ok
		},
	}
	return &pymoduleFixture{pp: pp, pool: pool, store: store, client: client}
}

// An executor that accepts syncs but carries no owner label is entitled to
// nobody's pymodules — the content is private per owner, so the absence of an
// owner label must skip the executor entirely: no owner resolution, no store
// read, no RPC.
func TestPymodulePusherSkipsExecutorWithNoOwnerLabel(t *testing.T) {
	live := []execpool.LiveExecutor{
		{Executor: executors.Executor{ID: "exec-noowner"}, Describe: &executorpb.DescribeResponse{PymodulesSync: true}},
	}
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{
		"u_alice": {{ID: 1, OwnerUserID: "u_alice", Name: "alice_chart", Code: "x"}},
	}}
	client := &fakePymoduleClient{}
	pp := &pymodulePusher{
		pool:    &fakePymodulePool{live: live, client: client},
		store:   store,
		version: "v1",
		resolveOwnerID: func(context.Context, string) (string, bool) {
			t.Error("resolveOwnerID must not be called for an executor with no owner label")
			return "", false
		},
	}

	le := live[0]
	if pp.eligible(le) {
		t.Error("an executor with no owner label must not be eligible for a pymodule push")
	}
	// The same executor WITH an owner label is eligible: the label is the gate,
	// not merely the Describe flag.
	le.Executor.Labels = map[string]string{"owner": "alice"}
	if !pp.eligible(le) {
		t.Error("an executor with an owner label and PymodulesSync must be eligible")
	}
	le.Executor.Labels = nil

	pp.pushAll(context.Background())
	if len(store.listed) != 0 {
		t.Errorf("store.List called for owner(s) %v; a no-owner executor must reach no store read", store.listed)
	}
	if len(client.requests) != 0 {
		t.Errorf("%d SyncPyModules RPC(s) attempted; a no-owner executor must receive none", len(client.requests))
	}
}

// The pusher is owner-scoped: pushing to one executor must read and send only
// THAT executor's owner's rows — never another owner's, and nothing at all
// for an owner whose username does not resolve.
func TestPymodulePusherRequestsOnlyTargetOwnersModules(t *testing.T) {
	f := newPymoduleFixture()

	// A direct push to exec-bob must read u_bob's rows only.
	if err := f.pp.pushTo(context.Background(), "exec-bob"); err != nil {
		t.Fatalf("pushTo(exec-bob) = %v, want nil", err)
	}
	if len(f.store.listed) != 1 || f.store.listed[0] != "u_bob" {
		t.Fatalf("store.List called for %v, want only [u_bob]", f.store.listed)
	}
	if len(f.client.requests) != 1 {
		t.Fatalf("%d RPC(s) attempted, want 1", len(f.client.requests))
	}
	if got := moduleNames(f.client.requests[0]); len(got) != 1 || got[0] != "bob_util" {
		t.Errorf("exec-bob received modules %v, want only bob's", got)
	}

	// An owner label that does not resolve pushes nothing at all.
	before := len(f.store.listed)
	if err := f.pp.pushTo(context.Background(), "exec-ghost"); err != nil {
		t.Fatalf("pushTo(exec-ghost) = %v, want nil", err)
	}
	if len(f.store.listed) != before {
		t.Errorf("store.List called %d more time(s) for an unresolvable owner", len(f.store.listed)-before)
	}
	if len(f.client.requests) != 1 {
		t.Errorf("an unresolvable owner must receive no RPC; got %d total", len(f.client.requests))
	}

	// The fan-out must still be per-owner: each executor gets only its own
	// owner's rows, resolved through its own label.
	f2 := newPymoduleFixture()
	f2.pp.pushAll(context.Background())
	if len(f2.store.listed) != 2 {
		t.Fatalf("store.List called %d time(s) across the fan-out, want 2 (one per owner)", len(f2.store.listed))
	}
	if f2.store.listed[0] != "u_alice" || f2.store.listed[1] != "u_bob" {
		t.Errorf("store.List called for %v, want [u_alice u_bob] — each executor its own owner", f2.store.listed)
	}
	if len(f2.client.requests) != 2 {
		t.Fatalf("%d RPC(s) across the fan-out, want 2", len(f2.client.requests))
	}
	if got := moduleNames(f2.client.requests[0]); len(got) != 1 || got[0] != "alice_chart" {
		t.Errorf("exec-alice received modules %v, want only alice's", got)
	}
	if got := moduleNames(f2.client.requests[1]); len(got) != 1 || got[0] != "bob_util" {
		t.Errorf("exec-bob received modules %v, want only bob's", got)
	}
}

// TestPymodulePusherBuildsSortedPayload is a shim so the verify pattern
// (-run TestPymodulePusher, an UNANCHORED substring match) also runs the
// payload-builder test, whose pinned name TestBuildPyModulesSortsByName does
// not contain the pattern.
func TestPymodulePusherBuildsSortedPayload(t *testing.T) {
	t.Run("sorts by name", TestBuildPyModulesSortsByName)
}

// buildPyModules is the payload builder. The executor compares content to
// decide whether to rewrite, so the module order must be deterministic
// (sorted by name) regardless of the order the store returned rows in.
func TestBuildPyModulesSortsByName(t *testing.T) {
	req := buildPyModules([]pymodules.Record{
		{Name: "zebra_plot", Code: "def zebra_plot(): pass"},
		{Name: "apple_util", Code: "def apple_util(): pass"},
		{Name: "mango_io", Code: "def mango_io(): pass"},
	}, "v7")

	if req.GetVersion() != "v7" {
		t.Errorf("version %q not stamped", req.GetVersion())
	}
	got := moduleNames(req)
	want := []string{"apple_util", "mango_io", "zebra_plot"}
	if !slices.Equal(got, want) {
		t.Errorf("modules = %v, want %v (sorted by name)", got, want)
	}
	codes := map[string]string{}
	for _, m := range req.GetModules() {
		codes[m.GetName()] = m.GetCode()
	}
	if codes["apple_util"] != "def apple_util(): pass" {
		t.Errorf("code not carried: %q", codes["apple_util"])
	}

	// Zero rows build an empty payload, not an error: an owner who has saved
	// nothing yet is a legitimate state, and syncing zero modules correctly
	// prunes that owner's cache dir (unlike skills, there is no
	// remove-everything hazard to guard against).
	if got := len(buildPyModules(nil, "v1").GetModules()); got != 0 {
		t.Errorf("empty corpus produced %d modules, want 0", got)
	}
}
