// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// sandboxSyncFixture wires a Controller and a pathSyncer over two REAL
// executors (exec-src and exec-dst, both owned by u-owner), plus a sandbox row
// named "box" pointing at exec-dst and created by the child c-child. It is what
// lets a test observe WHICH owner and caller child an adapter handed the
// syncer: "box" is reachable only to c-child, and exec-src only to u-owner.
type sandboxSyncFixture struct {
	ctrl *Controller
	p    *pathSyncer
	src  string
	dst  string
}

func newSandboxSyncFixture(t *testing.T) *sandboxSyncFixture {
	t.Helper()
	ck := assert.NewAborting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u-owner", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u-owner", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-src": srcClient,
		"exec-dst": dstClient,
	})

	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.SetPathSyncer(p)
	insertPathSyncChild(t, ctrl, "c-child", "", "")
	insertPathSyncChild(t, ctrl, "c-other", "", "")
	st := newFakeSandboxStore()
	ck.NoError(st.Insert(t.Context(), sandboxRowFor("sbx_box", "box", "u-owner", "exec-dst", "c-child")),
		"seed the sandbox row")
	ctrl.sandboxStore = st
	writeFile(t, filepath.Join(srcRoot, "hello.txt"), "hello", 0o644)

	return &sandboxSyncFixture{ctrl: ctrl, p: p, src: srcRoot, dst: dstRoot}
}

// syncArgs are the sandbox_sync arguments every call in these tests uses: copy
// one file from exec-src onto the sandbox "box".
func (f *sandboxSyncFixture) syncArgs() map[string]any {
	return map[string]any{
		"src_executor": "exec-src",
		"src_path":     filepath.Join(f.src, "hello.txt"),
		"dst_executor": "box",
		"dst_path":     filepath.Join(f.dst, "copy"),
	}
}

// TestMCPSandboxSyncToolsAppearForChildCredential pins that a daemon with a
// sandbox table exposes both transfer verbs on this face, for a user credential
// and a per-child credential alike — the same set the fundi binding grants.
func TestMCPSandboxSyncToolsAppearForChildCredential(t *testing.T) {
	c := assert.NewCollecting(t)
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = newFakeSandboxStore()

	requests := map[string]*http.Request{
		"user":  mcpRequestFor("u-owner"),
		"child": mcpChildRequest("u-owner", "c-child", false),
	}
	for provenance, req := range requests {
		names := mcpToolNames(t, mcpConnect(t, face.getServer(req)))
		for _, name := range []string{"sandbox_sync", "sandbox_sync_repo"} {
			c.Contains(names, name, "%s request is missing", provenance)
		}
	}
}

// TestMCPSandboxSyncAbsentWithoutStore pins the decline: a nil sandbox store
// materializes neither transfer verb, the same nil-means-decline rule the other
// sandbox blueprints follow.
func TestMCPSandboxSyncAbsentWithoutStore(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = nil

	names := mcpToolNames(t, mcpConnect(t, face.getServer(mcpRequestFor("u-owner"))))
	for _, name := range []string{"sandbox_sync", "sandbox_sync_repo"} {
		assert.NewCollecting(t).NotContains(names, name, "nil sandbox store unexpectedly exposes")
	}
}

// TestMCPSandboxSyncUnavailableWithoutSyncer pins the pool-less daemon: a
// daemon with a sandbox table but no path-sync backend answers both verbs with
// the explicit unavailable error, never a nil-pointer panic or a silent no-op.
func TestMCPSandboxSyncUnavailableWithoutSyncer(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = newFakeSandboxStore()
	cs := mcpConnect(t, face.getServer(mcpChildRequest("u-owner", "c-child", false)))

	calls := []struct {
		name string
		args map[string]any
	}{
		{"sandbox_sync", map[string]any{
			"src_executor": "exec-src", "src_path": "/src",
			"dst_executor": "box", "dst_path": "/dst",
		}},
		{"sandbox_sync_repo", map[string]any{
			"src_executor": "exec-src", "src_repo": "/r",
			"dst_executor": "box", "dst_repo": "/d", "branch": "main",
		}},
	}
	for _, tc := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		c.NoError(err, "%s transport", tc.name)
		c.True(res.IsError, "%s must fail without a syncer", tc.name)
		c.StrContains(mcpToolResultText(t, res), "path sync is not available on this daemon", "%s message", tc.name)
	}
}

// TestMCPSandboxSyncPassesCallerChildAndOwner drives sandbox_sync through the
// real face: the child that CREATED the sandbox reaches it, a sibling does not.
// "box" is reachable only to c-child and exec-src only to u-owner, so the
// success proves BOTH the caller's child id and the credential's owner reached
// the syncer; the sibling's refusal proves the child id is the caller's own,
// never a hardcoded or empty one.
func TestMCPSandboxSyncPassesCallerChildAndOwner(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	fx := newSandboxSyncFixture(t)
	face := newMCPFace(discardLogger(), nil, nil, "test")
	face.SetController(fx.ctrl)

	owner := mcpConnect(t, face.getServer(mcpChildRequest("u-owner", "c-child", false)))
	res, err := owner.CallTool(ctx, &mcp.CallToolParams{Name: "sandbox_sync", Arguments: fx.syncArgs()})
	c.NoError(err, "sandbox_sync over MCP")
	c.False(res.IsError, "the creating child must be allowed: %s", mcpToolResultText(t, res))
	c.StrContains(mcpCallText(t, res), "copied 1 files", "success text")

	sibling := mcpConnect(t, face.getServer(mcpChildRequest("u-owner", "c-other", false)))
	res, err = sibling.CallTool(ctx, &mcp.CallToolParams{Name: "sandbox_sync", Arguments: fx.syncArgs()})
	c.NoError(err, "sandbox_sync over MCP")
	c.True(res.IsError, "a sibling that did not create the sandbox must be refused")
	c.StrContains(mcpToolResultText(t, res), "not reachable", "sibling refusal")
}

// TestMCPSandboxSyncRepoPassesCallerChildAndOwner is the repo verb's twin: the
// creating child and the credential's owner reach the syncer (the call gets as
// far as "source is not a git repository", which is past BOTH resolves), while
// a sibling child id and a stranger owner are refused at resolution.
func TestMCPSandboxSyncRepoPassesCallerChildAndOwner(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	fx := newSandboxSyncFixture(t)

	req := protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "repo")},
		Dst:    protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "repo")},
		Branch: "main",
	}

	own := newMCPSandboxes(fx.ctrl, users.Identity{UserID: "u-owner"}, "c-child")
	_, err := own.SyncRepo(ctx, req)
	c.Error(err, "the source is not a git repository")
	c.StrContains(err.Error(), "source is not a git repository", "the call passed both resolves")
	c.NotStrContains(err.Error(), "not reachable", "a reachable sandbox must not be refused")

	sibling := newMCPSandboxes(fx.ctrl, users.Identity{UserID: "u-owner"}, "c-other")
	_, err = sibling.SyncRepo(ctx, req)
	c.Error(err, "a sibling must not reach the sandbox")
	c.StrContains(err.Error(), "not reachable", "sibling refusal")

	stranger := newMCPSandboxes(fx.ctrl, users.Identity{UserID: "u-stranger"}, "c-child")
	_, err = stranger.SyncRepo(ctx, req)
	c.Error(err, "a stranger owner must not reach the executors")
	c.StrContains(err.Error(), "not reachable", "owner refusal")
}

// TestMCPSandboxSyncMatchesConnectChildPolicy pins CLAUDE.md's "change both or
// neither": the Connect gate admits a per-child credential to SyncPath/SyncRepo
// (childScoped), and this face exposes exactly those two to the same credential.
// Widening or narrowing one plane alone diverges the two credential surfaces.
func TestMCPSandboxSyncMatchesConnectChildPolicy(t *testing.T) {
	c := assert.NewAborting(t)

	child := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-owner", ChildID: "c-child", Via: server.ProvenanceChildToken,
	})
	for _, proc := range []string{
		"/rafiki.v1.Control/SyncPath",
		"/rafiki.v1.Control/SyncRepo",
	} {
		c.Eq(policyChildScoped, policyFor(proc), "policy for %s", proc)
		c.NoError(authorizeControlProcedure(child, proc), "a child credential must be admitted to %s", proc)
	}

	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = newFakeSandboxStore()
	names := mcpToolNames(t, mcpConnect(t, face.getServer(mcpChildRequest("u-owner", "c-child", false))))
	for _, name := range []string{"sandbox_sync", "sandbox_sync_repo"} {
		c.Contains(names, name, "the MCP child credential is missing a verb the Connect plane admits")
	}
}
