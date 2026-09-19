// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/server"
)

// TestConnectPyModulesOwnerFromContext pins the security-relevant rule: the
// owner is resolved from the request CONTEXT, never from a request field. A
// ctx with no identity writes the shared unattributed bucket (owner ""), a
// ctx carrying an identity writes that identity's bucket, and a later List
// under the same identity sees only that owner's rows.
func TestConnectPyModulesOwnerFromContext(t *testing.T) {
	f := newPymoduleFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store}}

	// No identity on the ctx: the anonymous unix-socket case. The write lands
	// in the empty-owner bucket -- the shared unattributed bucket, never
	// "global".
	if _, err := m.PutPymodule(context.Background(), "shared_util", "x = 1", "nobody's util"); err != nil {
		t.Fatalf("PutPymodule with no identity: %v", err)
	}
	rows := f.store.rows[""]
	if len(rows) != 1 || rows[0].OwnerUserID != "" || rows[0].Name != "shared_util" {
		t.Fatalf("unattributed rows = %+v, want the write recorded under owner \"\"", rows)
	}

	// An identified ctx: the same call records THAT identity's UserID.
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u-alice"})
	if _, err := m.PutPymodule(alice, "alice_util", "def alice_util(): pass", "alice's util"); err != nil {
		t.Fatalf("PutPymodule as u-alice: %v", err)
	}
	if rows := f.store.rows["u-alice"]; len(rows) != 1 || rows[0].OwnerUserID != "u-alice" {
		t.Fatalf("u-alice rows = %+v, want the write recorded under owner \"u-alice\"", rows)
	}

	// Listing is owner-scoped the same way: alice's ctx returns only alice's
	// rows, and the anonymous ctx never sees them. Unfiltered (repo "") is
	// the default scope a caller gets.
	got, err := m.ListPymodules(alice, "")
	if err != nil {
		t.Fatalf("ListPymodules as u-alice: %v", err)
	}
	if len(got) != 1 || got[0].Name != "alice_util" {
		t.Fatalf("alice's list = %+v, want only [alice_util]", got)
	}
	anon, err := m.ListPymodules(context.Background(), "")
	if err != nil {
		t.Fatalf("ListPymodules with no identity: %v", err)
	}
	if len(anon) != 1 || anon[0].Name != "shared_util" {
		t.Fatalf("anonymous list = %+v, want only [shared_util]", anon)
	}
}

// TestConnectPyModulesPushesAfterWrite covers push-on-write through the
// adapter: a Put and a Delete each fan SyncPyModules RPCs out to the eligible
// executors, asserted on the real client's recorded requests -- the same
// assertion shape as TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush.
func TestConnectPyModulesPushesAfterWrite(t *testing.T) {
	f := newPymoduleFixture()
	// Per-executor clients attribute each payload to its executor: pushAll
	// fans out concurrently, so the shared client's arrival order is not
	// deterministic.
	aliceC, bobC := &fakePymoduleClient{}, &fakePymoduleClient{}
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": aliceC,
		"exec-bob":   bobC,
	}
	m := connectPyModules{c: &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}}
	// u_alice (underscore) is the fixture's alice bucket: the pusher resolves
	// exec-alice's owner label through the fixture's username map to exactly
	// that id, so the pushed payloads below carry this Put.
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	if _, err := m.PutPymodule(ctx, "alice_plot", "def alice_plot(): pass", "plots things"); err != nil {
		t.Fatalf("PutPymodule: %v", err)
	}
	// The put pushed: one SyncPyModules per eligible, resolvable executor
	// (exec-alice and exec-bob; exec-ghost's owner does not resolve).
	if len(aliceC.requests) != 1 || len(bobC.requests) != 1 {
		t.Fatalf("after Put: SyncPyModules requests = exec-alice %d, exec-bob %d; want 1 each", len(aliceC.requests), len(bobC.requests))
	}
	if got := moduleNames(aliceC.requests[0]); !containsName(got, "alice_plot") {
		t.Errorf("alice's push after Put = %v, want it to contain alice_plot", got)
	}

	if err := m.DeletePymodule(ctx, "alice_plot"); err != nil {
		t.Fatalf("DeletePymodule: %v", err)
	}
	// The delete pushed the pruned corpus too: one more RPC per executor,
	// and neither payload may still carry the deleted module.
	if len(aliceC.requests) != 2 || len(bobC.requests) != 2 {
		t.Fatalf("after Delete: SyncPyModules requests = exec-alice %d, exec-bob %d; want 2 each", len(aliceC.requests), len(bobC.requests))
	}
	for _, req := range []*executorpb.SyncPyModulesRequest{aliceC.requests[1], bobC.requests[1]} {
		if got := moduleNames(req); containsName(got, "alice_plot") {
			t.Errorf("push after Delete = %v, want the deleted module gone", got)
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// newGitListingFixture wires a connectPyModules over BOTH stores the list now
// spans: the blob fixture's store (alice_chart under u_alice, bob_util under
// u_bob) and the git-source fixture's pusher (whose cache a test populates by
// refreshing alice's registered sources).
func newGitListingFixture() (*pymoduleFixture, *gitSourceFixture, connectPyModules) {
	f := newPymoduleFixture()
	g := newGitSourceFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store, gitpymodulePusher: g.gp}}
	return f, g, m
}

// seedGitInventory refreshes the named source through the fixture so its
// discovery lands in the pusher's cache — the same way Task 3.2's inventory
// tests seed theirs. Only exec-alice serves alice's refreshes.
func seedGitInventory(g *gitSourceFixture, source string, resp *executorpb.SyncPyModuleGitSourceResponse) error {
	g.clients["exec-alice"].resp = resp
	recs, err := g.store.List(context.Background(), "u_alice")
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Name == source {
			_, err := g.gp.refresh(context.Background(), "u_alice", r.Name, r.URL, r.Ref)
			return err
		}
	}
	return gitpymodules.ErrNotFound
}

// TestConnectPyModulesListSpansGitSources pins the unfiltered list: local
// rows and git-sourced rows come back together, each stamped with its own
// Repo — "local" for the blob store, the source's name for its discovered
// scripts and packages — and git rows carry no version, no save time and no
// code. Owner scoping still holds: another owner's ctx sees neither alice's
// git rows nor her local ones.
func TestConnectPyModulesListSpansGitSources(t *testing.T) {
	_, g, m := newGitListingFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	if err := seedGitInventory(g, "ops_tools", &executorpb.SyncPyModuleGitSourceResponse{
		Scripts:   []*executorpb.GitSourceScript{{Name: "rotate_keys", Description: "rotates keys"}},
		Packages:  []*executorpb.GitSourcePackage{{Name: "opslib", Description: "ops helpers"}},
		VenvReady: true,
	}); err != nil {
		t.Fatalf("seed ops_tools: %v", err)
	}
	if err := seedGitInventory(g, "shared_lib", &executorpb.SyncPyModuleGitSourceResponse{
		Scripts: []*executorpb.GitSourceScript{{Name: "compile_helpers", Description: "compiles things"}},
	}); err != nil {
		t.Fatalf("seed shared_lib: %v", err)
	}

	rows, err := m.ListPymodules(alice, "")
	if err != nil {
		t.Fatalf("ListPymodules: %v", err)
	}
	byName := map[string]connectapi.PymoduleRow{}
	for _, r := range rows {
		if prev, seen := byName[r.Name]; seen {
			t.Fatalf("duplicate row name %q (%+v vs %+v)", r.Name, prev, r)
		}
		byName[r.Name] = r
	}
	// The local row, stamped as local.
	if r := byName["alice_chart"]; r.Repo != "local" || r.Version != 1 || r.Code != "" {
		t.Errorf("alice_chart = %+v, want Repo local, version 1, no code", r)
	}
	// ops_tools' script AND package, stamped with their source's name and
	// carrying neither version nor save time nor code.
	for _, want := range []struct{ name, description string }{{"rotate_keys", "rotates keys"}, {"opslib", "ops helpers"}} {
		r := byName[want.name]
		if r.Repo != "ops_tools" || r.Version != 0 || r.CreatedAt != "" || r.Code != "" || r.Description != want.description {
			t.Errorf("%s = %+v, want Repo ops_tools, zero version/createdAt/code, description %q", want.name, r, want.description)
		}
	}
	// shared_lib's script, under its own source's name.
	if r := byName["compile_helpers"]; r.Repo != "shared_lib" || r.Version != 0 || r.Code != "" {
		t.Errorf("compile_helpers = %+v, want Repo shared_lib with zero version/code", r)
	}
	if len(byName) != 4 {
		t.Errorf("got %d row(s) %v, want 4: alice_chart plus ops_tools' and shared_lib's discoveries", len(byName), rows)
	}

	// bob's ctx sees his own local row and nothing of alice's — not her git
	// rows either: the inventory cache is keyed per owner.
	bob := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_bob"})
	bobRows, err := m.ListPymodules(bob, "")
	if err != nil {
		t.Fatalf("ListPymodules as u_bob: %v", err)
	}
	if len(bobRows) != 1 || bobRows[0].Name != "bob_util" || bobRows[0].Repo != "local" {
		t.Errorf("bob's unfiltered list = %+v, want only his local bob_util", bobRows)
	}
}

// TestConnectPyModulesListFiltersToOneRepo pins the narrowing: a named git
// source returns ONLY that source's rows (no local rows, no other source's);
// "local" returns ONLY the blob-store rows; a name with no cached snapshot
// is an empty result, not an error.
func TestConnectPyModulesListFiltersToOneRepo(t *testing.T) {
	_, g, m := newGitListingFixture()
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})
	if err := seedGitInventory(g, "ops_tools", &executorpb.SyncPyModuleGitSourceResponse{
		Scripts:  []*executorpb.GitSourceScript{{Name: "rotate_keys"}},
		Packages: []*executorpb.GitSourcePackage{{Name: "opslib"}},
	}); err != nil {
		t.Fatalf("seed ops_tools: %v", err)
	}
	if err := seedGitInventory(g, "shared_lib", &executorpb.SyncPyModuleGitSourceResponse{
		Scripts: []*executorpb.GitSourceScript{{Name: "compile_helpers"}},
	}); err != nil {
		t.Fatalf("seed shared_lib: %v", err)
	}

	rows, err := m.ListPymodules(alice, "ops_tools")
	if err != nil {
		t.Fatalf("ListPymodules(ops_tools): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ops_tools rows = %+v, want exactly its two discoveries", rows)
	}
	for _, r := range rows {
		if r.Repo != "ops_tools" {
			t.Errorf("row %+v carries Repo %q, want ops_tools only", r, r.Repo)
		}
	}
	if got := []string{rows[0].Name, rows[1].Name}; got[0] != "rotate_keys" || got[1] != "opslib" {
		t.Errorf("rows = %v, want [rotate_keys opslib] (scripts before packages)", got)
	}

	// "local" narrows the other way: blob-store rows only, no git discoveries.
	localRows, err := m.ListPymodules(alice, "local")
	if err != nil {
		t.Fatalf("ListPymodules(local): %v", err)
	}
	if len(localRows) != 1 || localRows[0].Name != "alice_chart" || localRows[0].Repo != "local" {
		t.Errorf("local rows = %+v, want only alice_chart stamped local", localRows)
	}

	// An unrefreshed source name matches nothing — an empty list, not an error.
	empty, err := m.ListPymodules(alice, "never_refreshed")
	if err != nil {
		t.Fatalf("ListPymodules(never_refreshed): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unrefreshed scope returned %+v, want no rows", empty)
	}
}

// TestConnectPyModulesListSpansGitSourcesFiltersToOneRepo is a shim so the
// verify pattern (-run TestConnectPyModulesListSpansGitSources, an UNANCHORED
// substring match) also runs the single-scope filter test, whose pinned name
// does not contain the pattern — the same trick
// TestPymodulePusherBuildsSortedPayload uses for TestBuildPyModulesSortsByName.
func TestConnectPyModulesListSpansGitSourcesFiltersToOneRepo(t *testing.T) {
	t.Run("filters to one repo", TestConnectPyModulesListFiltersToOneRepo)
}

// TestConnectPyModulesNilPusherStillWrites pins the nil guard: a daemon with
// no executor pool constructs no pusher, and every write must still succeed
// -- the guard is what keeps the write path from panicking, and no RPC can
// have gone out because there is no pusher to make one.
func TestConnectPyModulesNilPusherStillWrites(t *testing.T) {
	f := newPymoduleFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store}} // pymodulePusher deliberately nil
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u-alice"})

	row, err := m.PutPymodule(ctx, "alice_util", "def alice_util(): pass", "alice's util")
	if err != nil {
		t.Fatalf("PutPymodule with nil pusher: %v", err)
	}
	if row.Version != 1 || row.Name != "alice_util" || row.Code != "def alice_util(): pass" {
		t.Errorf("PutPymodule row = %+v, want the saved record echoed back", row)
	}
	if err := m.DeletePymodule(ctx, "alice_util"); err != nil {
		t.Fatalf("DeletePymodule with nil pusher: %v", err)
	}
	if len(f.client.requests) != 0 {
		t.Errorf("SyncPyModules requests = %d, want 0 with no pusher configured", len(f.client.requests))
	}
}
