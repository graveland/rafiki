// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

// TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush covers the whole
// write path: a bound writer's Put lands in the store under ITS owner, then
// pushAll fans the new corpus out to the owner's executors -- and to nobody
// else's. The fake pusher is the real pymodulePusher over the fake pool and
// store fixture from pymodulesync_test.go, so the assertion is on actual
// SyncPyModules RPCs, not on a mock of the push.
func TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	// Per-executor clients attribute each payload to its executor: pushAll
	// fans out concurrently, so the shared client's arrival order is not
	// deterministic.
	aliceC, bobC := &fakePymoduleClient{}, &fakePymoduleClient{}
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": aliceC,
		"exec-bob":   bobC,
	}
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	id, _, err := w.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.Eq(2, id, "Put id") // alice's fake store already held one row (ID 1)

	// The put triggered a push: one SyncPyModules RPC per eligible executor.
	if len(aliceC.requests) != 1 || len(bobC.requests) != 1 {
		t.Fatalf("SyncPyModules requests: exec-alice %d, exec-bob %d; want 1 each (exec-alice and exec-bob)", len(aliceC.requests), len(bobC.requests))
	}
	// exec-alice (owned by alice) received alice's whole corpus, including the
	// module just saved.
	aliceNames := moduleNames(aliceC.requests[0])
	for _, want := range []string{"alice_chart", "alice_plot"} {
		c.Contains(aliceNames, want, "alice's push")
	}
	// exec-bob (owned by bob) received only bob's corpus: alice's new save
	// must not leak into another owner's push.
	if bobNames := moduleNames(bobC.requests[0]); slices.Contains(bobNames, "alice_plot") || !slices.Contains(bobNames, "bob_util") {
		t.Errorf("bob's push = %v, want [bob_util] only", bobNames)
	}
}

// A daemon with no executor pool constructs no pusher; a Put must still save
// (the child can list its own modules even though nothing syncs anywhere).
func TestNewControllerPyModuleWriterPutWithNilPusherStillSaves(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store} // pymodulePusher deliberately nil

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	id, _, err := w.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.Eq(2, id, "Put id")
	c.Empty(f.client.requests, "SyncPyModules requests = %d, want 0 with no pusher configured", len(f.client.requests))
}

// An anonymous spawn's empty owner is passed through to the store unchanged:
// pymodules.Store treats it as the shared unattributed bucket, never global.
func TestNewControllerPyModuleWriterPassesEmptyOwnerThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store}

	w := newControllerPyModuleWriter(ctrl, "")
	id, _, err := w.Put(context.Background(), "local", "unattributed_util", "x = 1", "nobody's util")
	c.Require().NoError(err, "Put")
	c.Eq(1, id, "Put id")
	rows := f.store.rows[""]
	c.False(len(rows) != 1 || rows[0].OwnerUserID != "" || rows[0].Name != "unattributed_util", "unattributed rows = %+v, want the save recorded under the empty owner", rows)
}

// Delete makes the in-memory store behave like the real one: the latest live
// row for (ownerUserID, name) disappears, an unknown or already-deleted name
// reports pymodules.ErrNotFound, and every call is recorded so a test can
// assert which owner a writer bound its delete to. It extends the fake defined
// in pymodulesync_test.go; only the method lives here.
func (s *fakePymoduleStore) Delete(_ context.Context, ownerUserID, name string) error {
	s.deleted = append(s.deleted, [2]string{ownerUserID, name})
	rows := s.rows[ownerUserID]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Name == name {
			s.rows[ownerUserID] = append(rows[:i:i], rows[i+1:]...)
			return nil
		}
	}
	return pymodules.ErrNotFound
}

// TestPymoduleDeleteTriggersOwnerScopedPush covers the delete write path: a
// bound writer's Delete lands in the store under ITS owner, then pushAll fans
// the pruned corpus out, so the next SyncPyModules for alice's executor no
// longer carries the deleted module and bob's payload never did. Assertions
// are on actual SyncPyModules RPC payloads, not on pusher mocks.
func TestPymoduleDeleteTriggersOwnerScopedPush(t *testing.T) {
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	// Per-executor clients attribute each payload to its executor: pushAll
	// fans out concurrently, so the shared client's arrival order is not
	// deterministic.
	aliceC, bobC := &fakePymoduleClient{}, &fakePymoduleClient{}
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": aliceC,
		"exec-bob":   bobC,
	}
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	if _, _, err := w.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The put's fan-out reached both eligible executors.
	if len(aliceC.requests) != 1 || len(bobC.requests) != 1 {
		t.Fatalf("after Put, SyncPyModules requests = exec-alice %d, exec-bob %d; want 1 each", len(aliceC.requests), len(bobC.requests))
	}

	_, err := w.Delete(context.Background(), "local", "alice_plot")
	assert.NewAborting(t).NoError(err, "Delete")
	// The delete reached the store under the writer's bound owner.
	if len(f.store.deleted) != 1 || f.store.deleted[0] != [2]string{"u_alice", "alice_plot"} {
		t.Fatalf("store Delete calls = %v, want exactly [{u_alice alice_plot}]", f.store.deleted)
	}
	// The delete pushed the pruned corpus: one more request per eligible executor.
	if len(aliceC.requests) != 2 || len(bobC.requests) != 2 {
		t.Fatalf("after Delete, SyncPyModules requests = exec-alice %d, exec-bob %d; want 2 each", len(aliceC.requests), len(bobC.requests))
	}
	// alice's next payload has dropped the deleted module and keeps the rest.
	if aliceNames := moduleNames(aliceC.requests[1]); slices.Contains(aliceNames, "alice_plot") || !slices.Contains(aliceNames, "alice_chart") {
		t.Errorf("alice's post-delete push = %v, want [alice_chart] without alice_plot", aliceNames)
	}
	// bob's payload (same fan-out) never contained alice's module.
	if bobNames := moduleNames(bobC.requests[1]); slices.Contains(bobNames, "alice_plot") || !slices.Contains(bobNames, "bob_util") {
		t.Errorf("bob's post-delete push = %v, want [bob_util] only", bobNames)
	}
}

// A failed delete must not push: the corpus did not change, so the fake
// client's request count is unchanged from before the delete.
func TestPymoduleDeleteNotFoundSkipsPush(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	before := len(f.client.requests)
	_, err := w.Delete(context.Background(), "local", "no_such_mod")
	c.Require().Error(err, "Delete of an unknown name = nil error, want not-found")
	c.ErrorIs(err, pymodules.ErrNotFound, "Delete error")
	c.Len(f.client.requests, before, "SyncPyModules requests = %d, want unchanged (%d): a failed delete must not push", len(f.client.requests), before)
}

// TestPymoduleInventoryRendersSavedModules renders the dynamic skill body.
func TestPymoduleInventoryRendersSavedModules(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	// A fresh store, not the shared fixture: the fixture pre-seeds rows, and
	// the empty-store case needs an owner with nothing saved.
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	ctrl := &Controller{pymoduleStore: store}

	// Empty store: a clear hint, not an empty string.
	body, err := pymoduleInventory(ctrl, "u_alice")(context.Background())
	c.Require().NoError(err, "inventory")
	c.Eq("No pymodules saved yet. Use pymodule_put to save one.", body, "empty-store inventory")

	if _, _, err := newControllerPyModuleWriter(ctrl, "u_alice").Put(context.Background(), "local", "alice_chart", "x = 1", "charts things"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, err = pymoduleInventory(ctrl, "u_alice")(context.Background())
	c.Require().NoError(err, "inventory")
	c.Eq("alice_chart — charts things\n", body, "inventory")
}

// TestPymoduleInventorySpansGitSources pins the skill body spanning both
// scopes: the owner's local rows first, then every git source's cached
// inventory, each git-sourced line labeled "reponame/name" so the agent can
// tell which repo value to pass back. A nil gitpymodulePusher (no exec pool)
// keeps the body to the local rows alone, never an error.
func TestPymoduleInventorySpansGitSources(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	// A fresh store, not the shared fixture: the fixture pre-seeds rows, and
	// the nil-pusher half below needs to see exactly the local rows.
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{
		"u_alice": {{ID: 1, OwnerUserID: "u_alice", Name: "alice_chart", Description: "charts things"}},
	}}
	gp := &gitPymodulePusher{cache: map[string]gitPymoduleInventory{
		gitSourceKey("u_alice", "ops-tools"): {
			Scripts:  []*executorpb.GitSourceScript{{Name: "rotate_keys", Description: "rotates the API keys"}},
			Packages: []*executorpb.GitSourcePackage{{Name: "opslib", Description: "ops helpers"}},
		},
	}}
	ctrl := &Controller{pymoduleStore: store, gitpymodulePusher: gp}

	body, err := pymoduleInventory(ctrl, "u_alice")(context.Background())
	c.Require().NoError(err, "inventory")
	for _, want := range []string{
		"alice_chart — charts things",                  // local row, bare name
		"ops-tools/rotate_keys — rotates the API keys", // discovered script, repo-labeled
		"ops-tools/opslib — ops helpers",               // discovered package, repo-labeled
	} {
		c.StrContains(body, want, "skill body")
	}

	// No exec pool: no pusher, so no git section and no error -- the body is
	// the local rows alone.
	nilCtrl := &Controller{pymoduleStore: store}
	body, err = pymoduleInventory(nilCtrl, "u_alice")(context.Background())
	c.Require().NoError(err, "inventory with nil pusher")
	c.NotStrContains(body, "ops-tools/", "skill body with nil pusher")
	c.StrContains(body, "alice_chart — charts things", "skill body with nil pusher")
}

// A bound writer's Get reads the latest live row under ITS owner: two
// versions under one name collapse to the higher-id row, and another owner's
// same-named module stays invisible.
func TestControllerPyModuleWriterGet(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	if _, _, err := w.Put(context.Background(), "local", "alice_chart", "x = 1", "v2 of alice's chart"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r, err := w.Get(context.Background(), "local", "alice_chart")
	c.Require().NoError(err, "Get")
	c.False(r.ID != 2 || r.Code != "x = 1", "Get = %+v, want the later version (id 2, code %q)", r, "x = 1")

	// A name owned by bob is invisible to alice's writer: the wrong owner
	// must not leak rows.
	_, err = w.Get(context.Background(), "local", "bob_util")
	c.ErrorIs(err, pymodules.ErrNotFound, "Get of a bob-owned name")
}

// A definite syntax error must block the save BEFORE the DB write: Put
// returns an error and the underlying pymodules.Store.Put is never reached
// (the fixture's seeded alice row count is unchanged). The error's
// OBSERVABLE shape is pinned at the tool layer: pymodulePutTool wraps every
// store error once with "pymodule_put: ", so the adapter must add none of
// its own and the end-to-end text is exactly
// "pymodule_put: <name> does not parse as Python: <syntax text>".
func TestControllerPyModuleWriterPutBlocksOnSyntaxError(t *testing.T) {
	c := assert.NewCollecting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	_, notice, err := w.Put(context.Background(), "local", "alice_bad", "def f(:\n    pass\n", "broken")
	c.Require().Error(err, "Put of syntactically invalid code = nil error, want a syntax-error failure")
	c.StrContains(err.Error(), "does not parse as Python", "Put error = %v, want it to name the parse failure", err)
	c.False(strings.HasPrefix(err.Error(), "pymodule_put:"), "Put error = %v, want no %q prefix at the adapter layer: the tool wraps store errors with it exactly once", err, "pymodule_put:")
	c.Eq("", notice, "Put notice")
	// The DB write never happened: alice still has exactly her seeded row.
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 1 || rows[0].Name != "alice_chart", "store rows = %+v, want only the seeded alice_chart: a syntax error must block before pymodules.Store.Put", rows)

	// The full text the calling agent sees: the same save through the real
	// pymodule_put tool, over this writer as its store.
	tool, err := tools.PyModulePutBlueprint{}.Materialize(tools.ToolOpts{PyModules: w})
	c.Require().NoError(err, "Materialize")
	input, err := json.Marshal(map[string]string{"repo": "local", "name": "alice_bad", "code": "def f(:\n    pass\n"})
	c.Require().NoError(err)
	_, err = tool.Execute(context.Background(), tools.ToolInput(input))
	c.Require().Error(err, "tool Execute of syntactically invalid code = nil error, want the syntax failure")
	const wantPrefix = "pymodule_put: alice_bad does not parse as Python: "
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("tool error = %q, want the end-to-end prefix %q exactly once", err.Error(), wantPrefix)
	}
	c.NotEq("", strings.TrimPrefix(err.Error(), wantPrefix), "tool error = %q, want the syntax text after %q to be non-empty", err.Error(), wantPrefix)
}

// A failed dependency install on one of the owner's executors does NOT block
// the save: the row is written and the failure rides back as the notice.
func TestControllerPyModuleWriterPutSurfacesVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": &fakePymoduleClient{venvResults: []*executorpb.PyModuleVenvResult{{Name: "alice_plot", Ready: false, Error: "boom"}}},
		"exec-bob":   &fakePymoduleClient{venvResults: []*executorpb.PyModuleVenvResult{{Name: "bob_util", Ready: true}}},
	}
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	id, notice, err := w.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.Eq(2, id, "Put id")
	c.StrContains(notice, "boom", "Put notice")
	// The save really happened in the store under the writer's owner.
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 2 || rows[1].Name != "alice_plot", "store rows = %+v, want alice_plot saved after the seeded alice_chart", rows)
}

// A lint finding is advisory, never blocking: the save goes through and the
// finding comes back in the notice. Hermetic -- RAFIKI_PYMODULE_UV points at
// the fake-findings uv from pymodule_checks_test.go, never a real ruff.
func TestControllerPyModuleWriterPutSucceedsDespiteLintFindings(t *testing.T) {
	c := assert.NewCollecting(t)
	uvDir := writeFakeLintUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	_, notice, err := w.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.StrContains(notice, fakeRuffFinding, "Put notice")
	c.StrContains(notice, "ruff found:", "Put notice")
	// The save went through despite the finding.
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 2 || rows[1].Name != "alice_plot", "store rows = %+v, want alice_plot saved: a lint finding must not block", rows)
}
