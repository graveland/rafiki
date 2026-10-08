// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func i64(v int64) *int64 { return &v }

// syncBlueprint returns the materialized sandbox_sync tool bound to mgr.
func syncTool(t *testing.T, mgr SandboxManager) Tool {
	t.Helper()
	tool, err := (&SandboxSyncBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	assert.NewAborting(t).NoError(err, "materialize sandbox_sync")
	return tool
}

// syncRepoTool returns the materialized sandbox_sync_repo tool bound to mgr.
func syncRepoTool(t *testing.T, mgr SandboxManager) Tool {
	t.Helper()
	tool, err := (&SandboxSyncRepoBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	assert.NewAborting(t).NoError(err, "materialize sandbox_sync_repo")
	return tool
}

// TestSandboxSyncToolDecodesArguments pins that sandbox_sync maps its flat
// arguments onto protocol.SyncPathRequest verbatim and hands it to the bound
// manager, and that the success text reports what moved.
func TestSandboxSyncToolDecodesArguments(t *testing.T) {
	c := assert.NewAborting(t)
	mgr := &fakeSandboxManager{syncRes: protocol.SyncPathResult{Files: 3, Bytes: 42}}

	res, err := syncTool(t, mgr).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst",`+
			`"overwrite":true,"max_bytes":5}`))
	c.NoError(err, "sandbox_sync")
	c.Eq("copied 3 files, 42 bytes", res.Text, "success text")

	c.Require().Len(mgr.syncReqs, 1, "the manager saw exactly one request")
	c.EqDeep(protocol.SyncPathRequest{
		Src:       protocol.SyncEndpoint{Executor: "host", Path: "/src"},
		Dst:       protocol.SyncEndpoint{Executor: "box", Path: "/dst"},
		Overwrite: true,
		MaxBytes:  i64(5),
	}, mgr.syncReqs[0], "the request handed to the manager")
}

// TestSandboxSyncToolMaxBytesOptional pins the optional max_bytes: absent is
// nil (no caller cap), present 5 is a cap of 5, and a present 0 is refused at
// the tool before the manager is ever called — zero meaning "unlimited" is the
// zero-value trap the wire shape exists to avoid.
func TestSandboxSyncToolMaxBytesOptional(t *testing.T) {
	c := assert.NewAborting(t)

	absent := &fakeSandboxManager{}
	if _, err := syncTool(t, absent).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst"}`)); err != nil {
		t.Fatalf("sandbox_sync without max_bytes: %v", err)
	}
	c.Require().Len(absent.syncReqs, 1, "one request")
	c.Nil(absent.syncReqs[0].MaxBytes, "an absent max_bytes must decode to nil, not zero")

	present := &fakeSandboxManager{}
	if _, err := syncTool(t, present).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst","max_bytes":5}`)); err != nil {
		t.Fatalf("sandbox_sync with max_bytes 5: %v", err)
	}
	c.Require().Len(present.syncReqs, 1, "one request")
	c.Require().NotNil(present.syncReqs[0].MaxBytes, "a present max_bytes must decode to a non-nil pointer")
	c.Eq(int64(5), *present.syncReqs[0].MaxBytes, "max_bytes value")

	zero := &fakeSandboxManager{}
	_, err := syncTool(t, zero).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst","max_bytes":0}`))
	c.Error(err, "a present max_bytes of 0 must be refused")
	c.StrContains(err.Error(), "max_bytes must be greater than zero", "refusal reason")
	c.Empty(zero.syncReqs, "a refused max_bytes must never reach the manager, got %d", len(zero.syncReqs))
}

// TestSandboxSyncToolOptionalBooleansDefaultFalse pins that an omitted
// overwrite/force decodes to false and a present one to true — neither is a
// silent default that flips on a missing field.
func TestSandboxSyncToolOptionalBooleansDefaultFalse(t *testing.T) {
	c := assert.NewAborting(t)

	absent := &fakeSandboxManager{}
	if _, err := syncTool(t, absent).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst"}`)); err != nil {
		t.Fatalf("sandbox_sync: %v", err)
	}
	c.False(absent.syncReqs[0].Overwrite, "an omitted overwrite must default to false")

	repoAbsent := &fakeSandboxManager{}
	if _, err := syncRepoTool(t, repoAbsent).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/r","dst_executor":"box","dst_repo":"/d","branch":"main"}`)); err != nil {
		t.Fatalf("sandbox_sync_repo: %v", err)
	}
	c.False(repoAbsent.repoReqs[0].Force, "an omitted force must default to false")

	repoPresent := &fakeSandboxManager{}
	if _, err := syncRepoTool(t, repoPresent).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/r","dst_executor":"box","dst_repo":"/d","branch":"main","force":true}`)); err != nil {
		t.Fatalf("sandbox_sync_repo force: %v", err)
	}
	c.True(repoPresent.repoReqs[0].Force, "a present force must be carried")
}

// TestSandboxSyncToolRejectsMissingRequiredFields pins the tool-side required
// check: an omitted executor or path is an error and never reaches the manager.
func TestSandboxSyncToolRejectsMissingRequiredFields(t *testing.T) {
	c := assert.NewCollecting(t)

	syncCases := map[string]string{
		"src_executor": `{"src_path":"/src","dst_executor":"box","dst_path":"/dst"}`,
		"src_path":     `{"src_executor":"host","dst_executor":"box","dst_path":"/dst"}`,
		"dst_executor": `{"src_executor":"host","src_path":"/src","dst_path":"/dst"}`,
		"dst_path":     `{"src_executor":"host","src_path":"/src","dst_executor":"box"}`,
	}
	for field, in := range syncCases {
		mgr := &fakeSandboxManager{}
		_, err := syncTool(t, mgr).Execute(context.Background(), ToolInput(in))
		c.Error(err, "sandbox_sync without %s", field)
		c.StrContains(err.Error(), field+" is required", "%s refusal", field)
		c.Empty(mgr.syncReqs, "sandbox_sync without %s reached the manager", field)
	}

	repoCases := map[string]string{
		"src_executor": `{"src_repo":"/r","dst_executor":"box","dst_repo":"/d","branch":"main"}`,
		"src_repo":     `{"src_executor":"host","dst_executor":"box","dst_repo":"/d","branch":"main"}`,
		"dst_executor": `{"src_executor":"host","src_repo":"/r","dst_repo":"/d","branch":"main"}`,
		"dst_repo":     `{"src_executor":"host","src_repo":"/r","dst_executor":"box","branch":"main"}`,
		"branch":       `{"src_executor":"host","src_repo":"/r","dst_executor":"box","dst_repo":"/d"}`,
	}
	for field, in := range repoCases {
		mgr := &fakeSandboxManager{}
		_, err := syncRepoTool(t, mgr).Execute(context.Background(), ToolInput(in))
		c.Error(err, "sandbox_sync_repo without %s", field)
		c.StrContains(err.Error(), field+" is required", "%s refusal", field)
		c.Empty(mgr.repoReqs, "sandbox_sync_repo without %s reached the manager", field)
	}
}

// TestSandboxSyncToolManagerErrorIsAToolError pins CLAUDE.md's rule: a manager
// failure is returned as an ERROR, never as a successful result carrying the
// text.
func TestSandboxSyncToolManagerErrorIsAToolError(t *testing.T) {
	c := assert.NewAborting(t)
	boom := errors.New("executor \"box\" is not reachable by this caller")

	_, err := syncTool(t, &fakeSandboxManager{syncErr: boom}).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_path":"/src","dst_executor":"box","dst_path":"/dst"}`))
	c.Error(err, "sandbox_sync must return the manager's error")
	c.StrContains(err.Error(), boom.Error(), "the error wraps the manager's")

	_, err = syncRepoTool(t, &fakeSandboxManager{repoErr: boom}).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/r","dst_executor":"box","dst_repo":"/d","branch":"main"}`))
	c.Error(err, "sandbox_sync_repo must return the manager's error")
	c.StrContains(err.Error(), boom.Error(), "the error wraps the manager's")
}

// TestSandboxSyncRepoToolDecodesArgumentsAndRendersResult pins the repo verb's
// mapping onto protocol.SyncRepoRequest and all three success texts: a created
// repository, an unchanged branch, and the abbreviated before..after ids.
func TestSandboxSyncRepoToolDecodesArgumentsAndRendersResult(t *testing.T) {
	c := assert.NewAborting(t)

	mgr := &fakeSandboxManager{repoRes: protocol.SyncRepoResult{
		OldOID: "0123456789abcdef0123456789abcdef01234567",
		NewOID: "fedcba9876543210fedcba9876543210fedcba98",
	}}
	res, err := syncRepoTool(t, mgr).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/src/repo","dst_executor":"box","dst_repo":"/dst/repo",`+
			`"branch":"main","force":true}`))
	c.NoError(err, "sandbox_sync_repo")
	c.Eq("0123456789ab..fedcba987654", res.Text, "updated text is the abbreviated old..new ids")
	c.EqDeep(protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "host", Path: "/src/repo"},
		Dst:    protocol.SyncEndpoint{Executor: "box", Path: "/dst/repo"},
		Branch: "main",
		Force:  true,
	}, mgr.repoReqs[0], "the request handed to the manager")

	created := &fakeSandboxManager{repoRes: protocol.SyncRepoResult{
		CreatedRepo: true, NewOID: "fedcba9876543210fedcba9876543210fedcba98",
	}}
	res, err = syncRepoTool(t, created).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/src/repo","dst_executor":"box","dst_repo":"/dst/repo","branch":"main"}`))
	c.NoError(err, "sandbox_sync_repo creating a repo")
	c.Eq("created repo", res.Text, "created text")

	uptodate := &fakeSandboxManager{repoRes: protocol.SyncRepoResult{
		UpToDate: true,
		OldOID:   "0123456789abcdef0123456789abcdef01234567",
		NewOID:   "0123456789abcdef0123456789abcdef01234567",
	}}
	res, err = syncRepoTool(t, uptodate).Execute(context.Background(), ToolInput(
		`{"src_executor":"host","src_repo":"/src/repo","dst_executor":"box","dst_repo":"/dst/repo","branch":"main"}`))
	c.NoError(err, "sandbox_sync_repo up to date")
	c.Eq("up to date", res.Text, "up-to-date text")
}

// TestSandboxSyncToolSchemasAndDescriptions pins the schemas' required lists and
// the two tool descriptions verbatim — a drifted description is a promise the
// tool no longer keeps (the overwrite/container rule, the checked-out-branch
// refusal).
func TestSandboxSyncToolSchemasAndDescriptions(t *testing.T) {
	c := assert.NewCollecting(t)

	sync := sandboxSchemaJSON(t, &SandboxSyncBlueprint{})
	c.EqDiff([]string{"src_executor", "src_path", "dst_executor", "dst_path"},
		toStrings(sync["required"].([]any)), "sandbox_sync required")
	c.Eq("Copy a file or directory between two executors you can reach (typically your host executor and a sandbox), brokered by the daemon. Paths are absolute and executor-local. overwrite replaces the destination and is only allowed on container executors.",
		(&SandboxSyncBlueprint{}).Description(), "sandbox_sync description")

	repo := sandboxSchemaJSON(t, &SandboxSyncRepoBlueprint{})
	c.EqDiff([]string{"src_executor", "src_repo", "dst_executor", "dst_repo", "branch"},
		toStrings(repo["required"].([]any)), "sandbox_sync_repo required")
	c.Eq("Fetch one git branch from a repository on one executor into a repository on another (git bundle underneath). Only committed state travels. Fast-forward only unless force; a branch that is checked out at the destination is refused.",
		(&SandboxSyncRepoBlueprint{}).Description(), "sandbox_sync_repo description")
}

// TestSandboxSyncToolsDeclineWithoutManager pins the nil-means-decline rule: a
// DB-less daemon materializes neither transfer verb.
func TestSandboxSyncToolsDeclineWithoutManager(t *testing.T) {
	c := assert.NewAborting(t)
	for _, bp := range []Tool{&SandboxSyncBlueprint{}, &SandboxSyncRepoBlueprint{}} {
		tool, err := bp.(Materializer).Materialize(ToolOpts{})
		c.NoError(err, "%s materialize", bp.Name())
		c.Nil(tool, "%s materialized with a nil manager", bp.Name())
	}
}

// TestSandboxSyncToolsAreRegisteredAndDaemonTier pins that both blueprints
// self-register and are classified TierDaemon: the transfer is brokered by the
// daemon, so it runs wherever the agent runs.
func TestSandboxSyncToolsAreRegisteredAndDaemonTier(t *testing.T) {
	c := assert.NewCollecting(t)
	registered := map[string]bool{}
	for _, bp := range DefaultBlueprint.All() {
		registered[bp.Name()] = true
	}
	for _, name := range []string{"sandbox_sync", "sandbox_sync_repo"} {
		c.True(registered[name], "%s is not registered in DefaultBlueprint", name)
		tier, ok := TierOf(name)
		c.True(ok, "%s has no tier", name)
		c.Eq(TierDaemon, tier, "%s tier", name)
	}
}
