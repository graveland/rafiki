// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

func TestMCPPyModuleStoreAdapter(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	ctx := context.Background()
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	ctrl := &Controller{pymoduleStore: store, pymodulePusher: nil}

	// Test Put with nil pusher
	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u-alice"})
	id, notice, err := mcp.Put(ctx, "local", "helper", "def f(): pass", "a helper")
	c.Require().NoError(err, "Put failed")
	c.Eq(1, id, "Put returned id")
	c.Eq("", notice, "Put notice")

	// Verify record is stored
	if records := store.rows["u-alice"]; len(records) != 1 {
		t.Errorf("fakePymoduleStore.rows[u-alice] has %d record(s), want 1", len(records))
	} else if records[0].Name != "helper" {
		t.Errorf("record Name is %q, want %q", records[0].Name, "helper")
	}

	// Test Delete with nil pusher
	if notice, err := mcp.Delete(ctx, "local", "helper"); err != nil || notice != "" {
		t.Fatalf("Delete = (%q, %v), want empty notice and nil error", notice, err)
	}

	// Verify deletion is tracked
	if len(store.deleted) != 1 {
		t.Errorf("fakePymoduleStore.deleted has %d entry/entries, want 1", len(store.deleted))
	} else if store.deleted[0] != [2]string{"u-alice", "helper"} {
		t.Errorf("deleted entry is %v, want [u-alice helper]", store.deleted[0])
	}
}

// The MCP face's Get is bound to its constructed owner: it serves that
// owner's latest live row and cannot see another owner's same-named module.
func TestMCPPyModuleStoreGet(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: nil}

	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u_alice"})
	r, err := mcp.Get(context.Background(), "local", "alice_chart")
	c.Require().NoError(err, "Get")
	c.False(r.ID != 1 || r.Name != "alice_chart" || r.Code != "def alice_chart(): pass", "Get = %+v, want alice's row (id 1)", r)

	_, err = mcp.Get(context.Background(), "local", "bob_util")
	c.ErrorIs(err, pymodules.ErrNotFound, "Get of a bob-owned name")
}

// The MCP face mirrors pymoduleWriter's ordering contract: a definite syntax
// error blocks BEFORE the DB write -- Put errors and the store is untouched.
// The error's OBSERVABLE shape is pinned at the tool layer (pymodulePutTool
// wraps store errors once with "pymodule_put: "), so the adapter adds none
// of its own and the end-to-end text is exactly
// "pymodule_put: <name> does not parse as Python: <syntax text>".
func TestMCPPyModuleStorePutBlocksOnSyntaxError(t *testing.T) {
	c := assert.NewCollecting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u_alice"})
	_, notice, err := mcp.Put(context.Background(), "local", "alice_bad", "def f(:\n    pass\n", "broken")
	c.Require().Error(err, "Put of syntactically invalid code = nil error, want a syntax-error failure")
	c.StrContains(err.Error(), "does not parse as Python", "Put error = %v, want it to name the parse failure", err)
	c.False(strings.HasPrefix(err.Error(), "pymodule_put:"), "Put error = %v, want no %q prefix at the adapter layer: the tool wraps store errors with it exactly once", err, "pymodule_put:")
	c.Eq("", notice, "Put notice")
	// The DB write never happened: alice still has exactly her seeded row.
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 1 || rows[0].Name != "alice_chart", "store rows = %+v, want only the seeded alice_chart: a syntax error must block before pymodules.Store.Put", rows)

	// The full text an MCP caller sees: the same save through the real
	// pymodule_put tool, over this store as its backend.
	tool, err := tools.PyModulePutBlueprint{}.Materialize(tools.ToolOpts{PyModules: mcp})
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
// the MCP face's save either: the row is written and the failure rides back
// as the notice.
func TestMCPPyModuleStorePutSurfacesVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	disableLint(t) // hermetic: this test does not exercise the lint outcome
	f := newPymoduleFixture()
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": &fakePymoduleClient{venvResults: []*executorpb.PyModuleVenvResult{{Name: "alice_plot", Ready: false, Error: "boom"}}},
		"exec-bob":   &fakePymoduleClient{venvResults: []*executorpb.PyModuleVenvResult{{Name: "bob_util", Ready: true}}},
	}
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u_alice"})
	id, notice, err := mcp.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.Eq(2, id, "Put id")
	c.StrContains(notice, "boom", "Put notice")
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 2 || rows[1].Name != "alice_plot", "store rows = %+v, want alice_plot saved after the seeded alice_chart", rows)
}

// A lint finding is advisory on the MCP face too: the save goes through and
// the finding comes back in the notice (hermetic fake uv, no real ruff).
func TestMCPPyModuleStorePutSucceedsDespiteLintFindings(t *testing.T) {
	c := assert.NewCollecting(t)
	uvDir := writeFakeLintUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u_alice"})
	_, notice, err := mcp.Put(context.Background(), "local", "alice_plot", "def alice_plot(): pass", "plots things")
	c.Require().NoError(err, "Put")
	c.StrContains(notice, fakeRuffFinding, "Put notice")
	c.StrContains(notice, "ruff found:", "Put notice")
	rows := f.store.rows["u_alice"]
	c.False(len(rows) != 2 || rows[1].Name != "alice_plot", "store rows = %+v, want alice_plot saved: a lint finding must not block", rows)
}

// gitInventoryFixture builds a Controller whose git pusher's cache holds two
// sources for u_alice -- one with a script and a package, one with a script
// of its own -- so the listing tests can assert both scoping directions. The
// pusher is cache-seeded directly (no pool, no refresh): allInventory and
// inventoryFor read the cache and nothing else.
func gitInventoryFixture(store *fakePymoduleStore) *Controller {
	gp := &gitPymodulePusher{cache: map[string]gitPymoduleInventory{
		gitSourceKey("u_alice", "ops-tools"): {
			Scripts:  []*executorpb.GitSourceScript{{Name: "rotate_keys", Description: "rotates the API keys"}},
			Packages: []*executorpb.GitSourcePackage{{Name: "opslib", Description: "ops helpers"}},
		},
		gitSourceKey("u_alice", "other-src"): {
			Scripts: []*executorpb.GitSourceScript{{Name: "other_tool", Description: "another source's tool"}},
		},
	}}
	return &Controller{pymoduleStore: store, gitpymodulePusher: gp}
}

// TestMCPPyModuleListSpansGitSources pins the MCP listing spanning both
// scopes when no repo filter is given: the local rows (bare names) plus every
// git source's entries (repo-labeled), the same shape the fundi skill body
// renders.
func TestMCPPyModuleListSpansGitSources(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{
		"u_alice": {{ID: 1, OwnerUserID: "u_alice", Name: "alice_chart", Description: "charts things"}},
	}}
	ctrl := gitInventoryFixture(store)
	lister := newMCPPyModuleLister(ctrl, users.Identity{UserID: "u_alice"})
	tool, err := mcpPyModuleListBlueprint{}.Materialize(tools.ToolOpts{PyModuleList: lister})
	c.Require().NoError(err, "Materialize")
	c.Require().NotNil(tool, "Materialize returned nil tool with a non-nil lister")

	// No arguments at all -- the unfiltered call -- spans both scopes.
	res, err := tool.Execute(context.Background(), tools.ToolInput{})
	c.Require().NoError(err, "Execute")
	for _, want := range []string{
		"alice_chart — charts things",                  // local, bare name
		"ops-tools/rotate_keys — rotates the API keys", // git script, repo-labeled
		"ops-tools/opslib — ops helpers",               // git package, repo-labeled
		"other-src/other_tool — another source's tool", // the second git source
	} {
		c.StrContains(res.Text, want, "pymodule_list result")
	}

	// An explicit empty repo is the same span, so a caller sending "repo": ""
	// gets everything and not a refusal.
	res, err = tool.Execute(context.Background(), tools.ToolInput(`{"repo":""}`))
	c.Require().NoError(err, "Execute(empty repo)")
	c.False(!strings.Contains(res.Text, "ops-tools/rotate_keys") || !strings.Contains(res.Text, "alice_chart"), "pymodule_list result with repo %q = %q, want both scopes again", "", res.Text)
}

// TestMCPPyModuleListFiltersByRepo pins the optional repo filter narrowing to
// one scope: a git source's name returns only that source's entries (no
// local rows, no other source), "local" returns only the blob store's.
func TestMCPPyModuleListFiltersByRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{
		"u_alice": {{ID: 1, OwnerUserID: "u_alice", Name: "alice_chart", Description: "charts things"}},
	}}
	ctrl := gitInventoryFixture(store)
	lister := newMCPPyModuleLister(ctrl, users.Identity{UserID: "u_alice"})
	tool, err := mcpPyModuleListBlueprint{}.Materialize(tools.ToolOpts{PyModuleList: lister})
	c.Require().NoError(err, "Materialize")

	res, err := tool.Execute(context.Background(), tools.ToolInput(`{"repo":"ops-tools"}`))
	c.Require().NoError(err, "Execute(ops-tools)")
	for _, want := range []string{"ops-tools/rotate_keys", "ops-tools/opslib"} {
		c.StrContains(res.Text, want, "pymodule_list(ops-tools)")
	}
	for _, banned := range []string{"alice_chart", "other-src/", "other_tool"} {
		c.NotStrContains(res.Text, banned, "pymodule_list(ops-tools)")
	}

	// "local" is itself one scope: only the blob store's rows, no git entries.
	res, err = tool.Execute(context.Background(), tools.ToolInput(`{"repo":"local"}`))
	c.Require().NoError(err, "Execute(local)")
	c.StrContains(res.Text, "alice_chart — charts things", "pymodule_list(local)")
	c.False(strings.Contains(res.Text, "ops-tools/") || strings.Contains(res.Text, "other-src/"), "pymodule_list(local) = %q, want no git-sourced entries", res.Text)

	// An unknown source name is an empty listing, not an error.
	res, err = tool.Execute(context.Background(), tools.ToolInput(`{"repo":"no-such-source"}`))
	c.Require().NoError(err, "Execute(no-such-source)")
	c.StrContains(res.Text, "No pymodules saved yet", "pymodule_list(no-such-source)")
}
