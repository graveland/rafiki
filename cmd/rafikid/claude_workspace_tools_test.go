// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// wsPtr is the pointer helper presets.WorkspaceTools needs.
func wsPtr(s string) *string { return &s }

// claudeWorkstation is the request shape the claude refusals below drive: a
// claude spawn that asked for rafiki's workspace tools, with a cwd that exists
// so nothing but the rule under test can refuse it.
func claudeWorkstation(t *testing.T) protocol.SpawnRequest {
	t.Helper()
	return protocol.SpawnRequest{
		Kind:           protocol.KindClaude,
		Cwd:            t.TempDir(),
		WorkspaceTools: protocol.WorkspaceToolsRafiki,
	}
}

// wantWorkspaceToolsRefusal asserts err is InvalidArgument carrying want.
func wantWorkspaceToolsRefusal(t *testing.T, err error, want string) {
	t.Helper()
	ck := assert.NewAborting(t)
	ck.Require().Error(err, "expected a refusal, got nil")
	cerr := ck.Require().ErrorAs[*connectapi.ControllerError](err)
	ck.Eq(protocol.ErrInvalidArgs, cerr.Code, "code")
	ck.Eq(want, cerr.Message, "message")
}

// TestWorkspaceToolsPresetResolvesOnce pins the resolve-once rule: the preset
// fills the mode at spawn, the child's row carries the RESOLVED value, and a
// resume reads that row. Editing the preset afterwards must not reach the
// resumed child — the same rule every other preset field follows.
func TestWorkspaceToolsPresetResolvesOnce(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1", presets.Record{
		Name:           "seat",
		Kind:           presets.KindClaude,
		WorkspaceTools: wsPtr(protocol.WorkspaceToolsRafiki),
	})
	ctrl := presetController(store)

	req, _, err := ctrl.applyPreset(context.Background(), protocol.SpawnRequest{Preset: "seat"}, "owner-1")
	ck.Require().NoError(err, "applyPreset")
	ck.Eq(protocol.WorkspaceToolsRafiki, req.WorkspaceTools, "the preset's mode at spawn")

	// The operator edits the preset after the child started.
	_, err = store.Put(context.Background(), "owner-1", presets.Record{
		Name:           "seat",
		Kind:           presets.KindClaude,
		WorkspaceTools: wsPtr(protocol.WorkspaceToolsNative),
	})
	ck.Require().NoError(err, "edit the preset")

	// What Spawn persisted is what resume rebuilds from.
	snap := childstore.Snapshot{Kind: protocol.KindClaude, WorkspaceTools: req.WorkspaceTools}
	resumed := resumeRequestFromSnapshot(snap, "")
	ck.Eq(protocol.WorkspaceToolsRafiki, resumed.WorkspaceTools,
		"resume must carry the row's mode, not the edited preset's")
}

// TestWorkspaceToolsRequestBeatsPreset pins the request-wins rule, and that
// "native" is stored as the empty default (NormalizeWorkspaceTools' one
// spelling of "the child keeps its built-ins").
func TestWorkspaceToolsRequestBeatsPreset(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1", presets.Record{
		Name:           "seat",
		Kind:           presets.KindClaude,
		WorkspaceTools: wsPtr(protocol.WorkspaceToolsNative),
	})
	ctrl := presetController(store)
	ctx := context.Background()

	// The request's rafiki choice survives a native preset.
	got, _, err := ctrl.applyPreset(ctx, protocol.SpawnRequest{
		Preset:         "seat",
		WorkspaceTools: protocol.WorkspaceToolsRafiki,
	}, "owner-1")
	ck.Require().NoError(err, "applyPreset")
	ck.Eq(protocol.WorkspaceToolsRafiki, got.WorkspaceTools, "the request's mode")

	// Nothing named: the preset's "native" arrives, and normalizes to "".
	got, _, err = ctrl.applyPreset(ctx, protocol.SpawnRequest{Preset: "seat"}, "owner-1")
	ck.Require().NoError(err, "applyPreset")
	ck.Eq(protocol.WorkspaceToolsNative, got.WorkspaceTools, "the preset's mode reaches the request verbatim")
	mode, ok := protocol.NormalizeWorkspaceTools(got.WorkspaceTools)
	ck.True(ok, "native must normalize")
	ck.Eq("", mode, "native must store as the empty default")
}

// TestWorkspaceToolsRefusedForFundi pins the first refusal: the mode replaces a
// claude child's built-ins, and a fundi child never had any.
func TestWorkspaceToolsRefusedForFundi(t *testing.T) {
	ctrl := newBatchSpawnController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.ShutdownAllChildren(ctx, time.Second, time.Second)
	})

	req := claudeWorkstation(t)
	req.Kind = protocol.KindFundi
	before := len(ctrl.st.List())
	_, err := ctrl.Spawn(context.Background(), req, users.Identity{})
	wantWorkspaceToolsRefusal(t, err, "workspace_tools applies to claude children; fundi always uses rafiki's workspace tools")
	assert.NewCollecting(t).Eq(before, len(ctrl.st.List()), "a refused spawn registers no child")
}

// TestWorkspaceToolsRefusedWithoutExecutorPool pins the second refusal: with no
// executor there is nowhere for the replacements to run, and running them in
// the daemon process is exactly what the executor architecture forbids.
func TestWorkspaceToolsRefusedWithoutExecutorPool(t *testing.T) {
	ctrl := newBatchSpawnController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.ShutdownAllChildren(ctx, time.Second, time.Second)
	})

	before := len(ctrl.st.List())
	_, err := ctrl.Spawn(context.Background(), claudeWorkstation(t), users.Identity{})
	wantWorkspaceToolsRefusal(t, err, `workspace_tools "rafiki" needs the executor pool: a claude child on the daemon host has no executor to route to`)
	assert.NewCollecting(t).Eq(before, len(ctrl.st.List()), "a refused spawn registers no child")
}

// TestWorkspaceToolsRefusedWithoutProxyURL pins the third refusal, added on the
// Wave-2.1 finding: darajaClaudeParams gates the whole mcp__rafiki__* surface on
// a configured proxy URL AND this kind being proxied at all, so either miss
// leaves the eight built-ins disallowed with nothing replacing them — a
// toolless child.
func TestWorkspaceToolsRefusedWithoutProxyURL(t *testing.T) {
	const want = `workspace_tools "rafiki" needs the MCP surface: this daemon serves no proxy for claude children (no proxy URL, or claude outside RAFIKI_PROXY_KINDS), so the built-in workspace tools would be disallowed and unreplaced`

	for _, tc := range []struct {
		name       string
		proxyURL   string
		proxyKinds string
	}{
		{name: "no proxy URL", proxyURL: "", proxyKinds: ""},
		{name: "claude outside the proxy kinds", proxyURL: "http://127.0.0.1:1", proxyKinds: "fundi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RAFIKI_URL", "")
			t.Setenv("RAFIKI_PROXY_KINDS", tc.proxyKinds)
			ctrl := newBatchSpawnController(t)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = ctrl.ShutdownAllChildren(ctx, time.Second, time.Second)
			})
			// An executor pool IS configured: only the MCP surface is missing.
			ctrl.execPoolConn = &execpool.Pool{}
			ctrl.darajaPool = &darajapool.Pool{}
			ctrl.proxyURL = tc.proxyURL
			ctrl.proxyToken = "tok"

			before := len(ctrl.st.List())
			_, err := ctrl.Spawn(context.Background(), claudeWorkstation(t), users.Identity{})
			wantWorkspaceToolsRefusal(t, err, want)
			assert.NewCollecting(t).Eq(before, len(ctrl.st.List()), "a refused spawn registers no child")
		})
	}
}

// TestWorkspaceToolsDefaultsToRafikiWhereServable pins the DEFAULT, which is
// the resolution's other half: a claude spawn that names no mode gets rafiki's
// executor-routed workspace tools wherever this daemon can actually serve them,
// and Claude Code's built-ins only where it cannot (no executor pool, or no MCP
// face for claude children). The default is a capability question rather than a
// preference, so the fallback NEVER refuses a spawn nobody declared a mode for
// — that is what the pin tests above are for.
//
// An explicit "native" outranks the default on a servable daemon too: a pin is
// a decision, and the daemon does not get to re-make it.
func TestWorkspaceToolsDefaultsToRafikiWhereServable(t *testing.T) {
	const (
		poolMissing = `workspace_tools "rafiki" needs the executor pool: a claude child on the daemon host has no executor to route to`
		noSurface   = `workspace_tools "rafiki" needs the MCP surface: this daemon serves no proxy for claude children (no proxy URL, or claude outside RAFIKI_PROXY_KINDS), so the built-in workspace tools would be disallowed and unreplaced`
		fundiRefuse = `workspace_tools applies to claude children; fundi always uses rafiki's workspace tools`
		invalid     = `workspace_tools "bogus" is not one of "native", "rafiki"`
	)

	for _, tc := range []struct {
		name       string
		pool       bool
		proxyURL   string
		proxyKinds string
		kind       string
		requested  string
		want       string
		wantErr    string
		// wantDegraded: the default fell back on a daemon that HAS the executor
		// pool, so the missing piece is the MCP surface and Spawn logs it. A
		// pool-less daemon (the laptop shape) falls back quietly.
		wantDegraded bool
	}{
		// The default, on every topology.
		{name: "no executor pool keeps the built-ins", kind: protocol.KindClaude, want: ""},
		{name: "a pool with no proxy URL is reported", pool: true, kind: protocol.KindClaude, want: "", wantDegraded: true},
		{name: "claude outside the proxy kinds keeps the built-ins and is reported", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "fundi", kind: protocol.KindClaude, want: "", wantDegraded: true},
		{name: "a servable daemon defaults to rafiki", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindClaude, want: protocol.WorkspaceToolsRafiki},
		// A pin is never re-made.
		{name: "explicit native on a servable daemon", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindClaude, requested: protocol.WorkspaceToolsNative, want: ""},
		// A declared mode is never the default, so nothing degraded.
		{name: "explicit native on a broken daemon is not a degraded default", pool: true, kind: protocol.KindClaude, requested: protocol.WorkspaceToolsNative, want: ""},
		{name: "explicit rafiki on a servable daemon", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindClaude, requested: protocol.WorkspaceToolsRafiki, want: protocol.WorkspaceToolsRafiki},
		// Pins the daemon cannot honour are refused, never downgraded.
		{name: "explicit rafiki without a pool", kind: protocol.KindClaude, requested: protocol.WorkspaceToolsRafiki, wantErr: poolMissing},
		{name: "explicit rafiki without a surface", pool: true, kind: protocol.KindClaude, requested: protocol.WorkspaceToolsRafiki, wantErr: noSurface},
		{name: "rafiki is meaningless for fundi", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindFundi, requested: protocol.WorkspaceToolsRafiki, wantErr: fundiRefuse},
		{name: "rafiki is meaningless for a script", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindScript, requested: protocol.WorkspaceToolsRafiki, wantErr: fundiRefuse},
		// A fundi spawn that names nothing is not given a claude-only mode.
		{name: "fundi has no default", pool: true, proxyURL: "http://127.0.0.1:1", proxyKinds: "claude", kind: protocol.KindFundi, want: ""},
		{name: "an unknown value is invalid", kind: protocol.KindClaude, requested: "bogus", wantErr: invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			t.Setenv("RAFIKI_URL", "")
			t.Setenv("RAFIKI_PROXY_KINDS", tc.proxyKinds)
			ctrl := &Controller{}
			if tc.pool {
				ctrl.execPoolConn = &execpool.Pool{}
				ctrl.darajaPool = &darajapool.Pool{}
			}
			ctrl.proxyURL = tc.proxyURL
			ctrl.proxyToken = "tok"

			mode, refusal, degraded := ctrl.resolveWorkspaceTools(tc.kind, tc.requested)
			ck.Eq(tc.wantErr, refusal, "refusal")
			ck.Eq(tc.wantDegraded, degraded != "", "degraded reason reported")
			if tc.wantErr != "" {
				return
			}
			ck.Eq(tc.want, mode, "resolved mode")
		})
	}
}

// TestWorkspaceToolsDefaultOnAnUnservableDaemonStoresTheBuiltins pins the
// default on the path that matters — the mode Spawn persists on the child's row,
// which is what a resume rebuilds from and what the MCP face reads when deciding
// to serve the replacement tools — for the one topology a unit test can drive
// end to end. A default resolved into a local variable only would leave the row
// disagreeing with the launch.
//
// Scope: this covers the FALLBACK half. The row carrying `rafiki` is pinned
// end-to-end by TestWorkspaceToolsRafikiClaudeChildReportsTheModeItSpawnedWith
// (test/integration), whose daemon really has the pool and the proxy face —
// a unit-test controller with a pool cannot launch the daraja a rafiki-mode
// child needs.
func TestWorkspaceToolsDefaultOnAnUnservableDaemonStoresTheBuiltins(t *testing.T) {
	ck := assert.NewAborting(t)
	// newTestController configures no executor pool, so this daemon cannot
	// serve rafiki mode and the default settles on the built-ins.
	ctrl := newTestController(t)

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn a claude child that named no mode")
	ck.Eq("", mustSnapshot(t, ctrl, res.ChildID).WorkspaceTools,
		"a daemon that cannot serve rafiki mode must store the built-ins")
}

// TestWorkspaceToolsResumeRefusedWhenTheDaemonCannotServe pins the resume half
// of the DEFAULT: a row carrying `rafiki` is relaunched with Claude Code's
// built-ins disallowed and rafiki's replacements served in their place, and a
// resume never re-resolves the mode — so a daemon that lost the pool or the MCP
// surface since the spawn would otherwise bring the child back TOOLLESS, with
// its row still claiming a mode nothing serves. It must refuse instead, naming
// what to restore and the pin a replacement can use.
func TestWorkspaceToolsResumeRefusedWhenTheDaemonCannotServe(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctx := t.Context()

	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)

	// The row claims rafiki mode, and this controller has no executor pool.
	ck.Require().NoError(ctrl.st.Update(res.ChildID, func(s *childstore.Session) {
		s.WorkspaceTools = protocol.WorkspaceToolsRafiki
	}), "set mode")

	_, err = ctrl.Resume(ctx, res.ChildID, "")
	ck.Require().Error(err, "resume of an unservable rafiki-mode child must be refused")
	cerr := ck.Require().ErrorAs[*connectapi.ControllerError](err)
	ck.Eq(protocol.ErrNotResumable, cerr.Code, "code")
	ck.StrContains(cerr.Message, `workspace_tools "rafiki"`, "the refusal names the row's mode")
	ck.StrContains(cerr.Message, `workspace_tools "native"`, "the refusal names the remedy")
	// Nothing was relaunched: the row keeps its own mode and stays exited.
	snap := mustSnapshot(t, ctrl, res.ChildID)
	ck.Eq(protocol.WorkspaceToolsRafiki, snap.WorkspaceTools, "a refused resume must not rewrite the row")
	ck.Eq(protocol.StatusExited, snap.Status, "a refused resume must not relaunch the child")
}

// TestWorkspaceToolsResumeRefusalIgnoresServableAndNativeChildren pins the two
// cases the refusal must stay out of: a native child (nothing to serve) and a
// rafiki child on a daemon that can still serve it.
func TestWorkspaceToolsResumeRefusalIgnoresServableAndNativeChildren(t *testing.T) {
	ck := assert.NewAborting(t)
	t.Setenv("RAFIKI_URL", "")
	t.Setenv("RAFIKI_PROXY_KINDS", "claude")
	servable := &Controller{
		execPoolConn: &execpool.Pool{},
		darajaPool:   &darajapool.Pool{},
		proxyURL:     "http://127.0.0.1:1",
		proxyToken:   "tok",
	}
	ck.Eq("", servable.workspaceToolsResumeRefusal(protocol.KindClaude, protocol.WorkspaceToolsRafiki),
		"a servable daemon resumes a rafiki child")
	ck.Eq("", servable.workspaceToolsResumeRefusal(protocol.KindClaude, ""),
		"a native child never needs the refusal")
	ck.Eq("", (&Controller{}).workspaceToolsResumeRefusal(protocol.KindClaude, ""),
		"a native child on a bare daemon too")
	ck.NotEq("", (&Controller{}).workspaceToolsResumeRefusal(protocol.KindClaude, protocol.WorkspaceToolsRafiki),
		"a rafiki child on a daemon with no pool must be refused")
	ck.Eq("", (&Controller{}).workspaceToolsResumeRefusal(protocol.KindFundi, protocol.WorkspaceToolsRafiki),
		"only a claude child carries the mode")
}

// TestWorkspaceToolsCarriedOnResume pins the resume half of the plumbing: the
// request every resume site rebuilds from the snapshot must carry the mode, or
// a resumed child relaunches native while its MCP replacements stay served.
func TestWorkspaceToolsCarriedOnResume(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, want := range []string{"", protocol.WorkspaceToolsRafiki} {
		req := resumeRequestFromSnapshot(childstore.Snapshot{
			Kind:           protocol.KindClaude,
			WorkspaceTools: want,
		}, "")
		ck.Eq(want, req.WorkspaceTools, "resume request WorkspaceTools for mode %q", want)
	}
}

// The other resume half — the Session activateLiveChild inserts (and persists)
// keeps the mode — is the same one-line copy from the snapshot, and it cannot
// be reached from here any more: a rafiki-mode row may only be resumed by a
// daemon that can SERVE it (workspaceToolsResumeRefusal), and a unit-test
// controller with an executor pool cannot launch a daraja. It is pinned
// end-to-end instead, by the kill-and-resume round trip in
// TestWorkspaceToolsRafikiClaudeChildReportsTheModeItSpawnedWith
// (test/integration): dropping `WorkspaceTools: snap.WorkspaceTools` from the
// resume literal makes that resumed child report "".

// TestWorkspaceToolsClaudeChildGetsBoundExecutorHomedOnLaunchHost pins the
// binding claudeRunner registers for a rafiki-mode child: a boundExecutor built
// from the child's own request and owner, homed on the executor its daraja was
// launched on, retained where the MCP face looks it up. A native child gets
// none — its built-in tools already run on that machine.
//
// Driven through bindClaudeWorkspaceTools rather than claudeRunner because
// darajapool.Launch is not injectable: this is the piece claudeRunner runs
// after a successful launch.
func TestWorkspaceToolsClaudeChildGetsBoundExecutorHomedOnLaunchHost(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := &Controller{st: childstore.New(), cm: newChildManager()}

	req := protocol.SpawnRequest{
		Kind:             protocol.KindClaude,
		WorkspaceTools:   protocol.WorkspaceToolsRafiki,
		ExecutorSelector: "machine=box-1",
	}
	ctrl.bindClaudeWorkspaceTools(req, "c_ws", executors.Executor{ID: "exec-1"}, executorOwner{Name: "brent", UserID: "u-1"})

	be := ctrl.boundExecutorFor("c_ws")
	ck.Require().NotNil(be, "a rafiki-mode claude child got no boundExecutor")
	ck.Eq("exec-1", be.homeExecutor(), "the binding's home must be the launch executor")

	// The binder is built from THIS child's request and owner, not a default.
	cb, ok := be.binder.(*controllerBinder)
	ck.Require().True(ok, "binder is %T, want *controllerBinder", be.binder)
	ck.Eq("machine=box-1", cb.req.ExecutorSelector, "binder request")
	ck.Eq("brent", cb.owner.Name, "binder owner name")
	ck.Eq("u-1", cb.owner.UserID, "binder owner user id")

	// A native child gets no binding at all.
	ctrl.bindClaudeWorkspaceTools(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c_native", executors.Executor{ID: "exec-1"}, executorOwner{})
	ck.Nil(ctrl.boundExecutorFor("c_native"), "a native claude child must get no boundExecutor")
}

// mcpWorkspaceChildRequest returns the child-provenance request the face sees
// for a claude child, after seeding its row (mode, cwd, launch labels) and the
// binding claudeRunner would have registered.
func mcpWorkspaceChildRequest(t *testing.T, face *mcpFace, mode string, binder executorBinder) *http.Request {
	t.Helper()
	ctrl := face.controller()
	ctrl.pymoduleStore = &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	ctrl.execPoolConn = joinLiveRelayExecutor(t, executors.Executor{ID: "e-1"}, nil)
	ctrl.darajaPool = &darajapool.Pool{}
	ctrl.st.Insert(&childstore.Session{
		ChildID:        "c-child",
		Cwd:            "/work",
		WorkspaceTools: mode,
		Labels:         map[string]string{"rafiki/executor": "e-1"},
	})
	if binder != nil {
		ctrl.noteBoundExecutor("c-child", newBoundExecutor("c-child", binder))
	}

	r := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	ctx := server.WithIdentity(r.Context(), &server.Identity{
		UserID: "u-owner", ChildID: "c-child", Via: server.ProvenanceChildToken,
	})
	return r.WithContext(ctx)
}

// mcpToolsByName lists the server's tools keyed by name, failing on a duplicate
// — which is what pins "exactly one pymodule_run".
func mcpToolsByName(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	ck := assert.NewAborting(t)
	res, err := cs.ListTools(context.Background(), nil)
	ck.Require().NoError(err, "ListTools")
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tl := range res.Tools {
		if _, dup := out[tl.Name]; dup {
			t.Fatalf("tool %q is listed more than once; exactly one must be served", tl.Name)
		}
		out[tl.Name] = tl
	}
	return out
}

// schemaHasExecutor reports whether a listed tool's input schema carries the
// per-call `executor` property.
func schemaHasExecutor(t *testing.T, tl *mcp.Tool) bool {
	t.Helper()
	ck := assert.NewAborting(t)
	schema, ok := tl.InputSchema.(map[string]any)
	ck.Require().True(ok, "tool %q schema is %T, want an object", tl.Name, tl.InputSchema)
	props, ok := schema["properties"].(map[string]any)
	ck.Require().True(ok, "tool %q schema has no properties object", tl.Name)
	_, has := props["executor"]
	return has
}

// lspToolNames is the eight lsp_* replacements for Claude Code's built-in LSP
// tool, which `workspace_tools: "rafiki"` disallows. All eight are
// TierWorkspace in pkg/fundi/tools, and the executor's Describe omits them when
// it has no language server.
var lspToolNames = []string{
	"lsp_call_hierarchy",
	"lsp_definition",
	"lsp_diagnostics",
	"lsp_references",
	"lsp_rename",
	"lsp_replace_symbol",
	"lsp_restart",
	"lsp_symbols",
}

// workspaceTierNames is the whole workspace tier a rafiki-mode claude child is
// served on this face: Claude Code's own four spellings, the search/list verbs,
// the job verbs and lsp_*. Both the rafiki-mode test (all served) and the
// native-mode test (none served) read it, so a tool added to the face's tier is
// checked against a native child too.
var workspaceTierNames = func() []string {
	out := []string{"Bash", "Edit", "Read", "Write", "bash_kill", "bash_output", "glob", "grep", "ls"}
	out = append(out, lspToolNames...)
	slices.Sort(out)
	return out
}()

// TestMCPFaceWorkspaceToolsRafikiChildSeesClaudeShaped pins the served surface
// of a rafiki-mode claude child: Claude Code's four spellings, the
// search/list/job verbs and the eight lsp_* replacements, each with the
// per-call `executor` argument; none of the native four; and exactly one
// pymodule_run (the child-binding one the face already served).
func TestMCPFaceWorkspaceToolsRafikiChildSeesClaudeShaped(t *testing.T) {
	ck := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	req := mcpWorkspaceChildRequest(t, face, protocol.WorkspaceToolsRafiki, newFakeBinder(&fakeExec{id: "e-1"}))

	toolsByName := mcpToolsByName(t, mcpConnect(t, face.getServer(req)))

	for _, name := range workspaceTierNames {
		tl, ok := toolsByName[name]
		ck.Require().True(ok, "rafiki-mode child is missing %q (has %v)", name, sortedToolNames(toolsByName))
		ck.True(schemaHasExecutor(t, tl), "workspace tool %q must carry the executor argument", name)
	}
	for _, name := range []string{"read", "write", "edit", "bash"} {
		ck.NotContains(sortedToolNames(toolsByName), name,
			"rafiki-mode child was offered the native spelling %q as well as the Claude-shaped one", name)
	}
	ck.Contains(sortedToolNames(toolsByName), "pymodule_run", "the child-binding pymodule_run must still be served")
	ck.Len(countName(sortedToolNames(toolsByName), "pymodule_run"), 1, "exactly one pymodule_run")
}

// sortedToolNames is the deterministic name list for Contains/NotContains.
func sortedToolNames(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// countName returns every occurrence of name in names.
func countName(names []string, name string) []string {
	var out []string
	for _, n := range names {
		if n == name {
			out = append(out, n)
		}
	}
	return out
}

// TestMCPFaceWorkspaceToolsNativeChildUnchanged pins that a native (or
// unspecified) child's surface is untouched: no workspace tier joins this face
// for it, even when a binding happens to be retained.
func TestMCPFaceWorkspaceToolsNativeChildUnchanged(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, mode := range []string{"", protocol.WorkspaceToolsNative} {
		face, _ := mcpFaceFixture(t)
		req := mcpWorkspaceChildRequest(t, face, mode, newFakeBinder(&fakeExec{id: "e-1"}))
		names := sortedToolNames(mcpToolsByName(t, mcpConnect(t, face.getServer(req))))
		for _, name := range workspaceTierNames {
			ck.NotContains(names, name, "native child (mode %q) unexpectedly got the workspace tier", mode)
		}
		ck.Contains(names, "task_add", "native child (mode %q) lost the ordinary surface", mode)
	}
}

// TestMCPFaceWorkspaceToolsOmitsWhatTheExecutorDoesNotServe pins the served-set
// filter over the LSP replacements, which is the case it exists for: a machine
// with no configured language server omits lsp_* from its Describe (pkg/executor
// builds its registry from the installed servers), and the face must then hand
// the child none of them rather than eight tools that can only fail. The other
// direction is pinned beside it, so the filter cannot be satisfied by dropping
// lsp_* unconditionally. The Claude-shaped four are daemon-side translators and
// never routed, so no served set can drop them; the parent-side job verbs are
// RPCs and are not routed either.
func TestMCPFaceWorkspaceToolsOmitsWhatTheExecutorDoesNotServe(t *testing.T) {
	fileTools := []string{"read", "write", "edit", "glob", "grep", "ls", "bash"}
	for _, tc := range []struct {
		name    string
		served  []string
		wantLSP bool
	}{
		{name: "machine with no language server", served: fileTools},
		{
			name:    "machine with a language server",
			served:  append(append([]string{}, fileTools...), lspToolNames...),
			wantLSP: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			face, _ := mcpFaceFixture(t)
			// homeLabel is what be.homeExecutor() seeds from; it names the
			// executor the served set is read for.
			binder := newFakeBinder(&fakeExec{id: "e-1"})
			binder.homeLabel = "e-1"
			req := mcpWorkspaceChildRequest(t, face, protocol.WorkspaceToolsRafiki, binder)

			// Exactly what this machine's Describe reports.
			face.controller().execPool = &fakePool{live: []execpool.LiveExecutor{{
				Executor: executors.Executor{ID: "e-1"},
				Describe: &executorpb.DescribeResponse{Tools: tc.served},
			}}}

			names := sortedToolNames(mcpToolsByName(t, mcpConnect(t, face.getServer(req))))
			for _, name := range lspToolNames {
				if tc.wantLSP {
					ck.Contains(names, name, "this machine serves %q, so it must be advertised (has %v)", name, names)
					continue
				}
				ck.NotContains(names, name,
					"this machine serves no language server, yet %q was advertised (has %v)", name, names)
			}
			for _, name := range []string{"Read", "Write", "Edit", "Bash", "glob", "grep", "ls", "bash_output", "bash_kill"} {
				ck.Contains(names, name, "the served-set filter dropped %q, which this machine reports serving", name)
			}
		})
	}
}

// TestMCPFaceWorkspaceToolsAmbiguityError pins that a workspace call reaches the
// binding's own resolution: with two reachable executors and no `executor`
// argument, Read must be refused with the binding's ambiguity text rather than
// silently picking one.
func TestMCPFaceWorkspaceToolsAmbiguityError(t *testing.T) {
	ck := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	req := mcpWorkspaceChildRequest(t, face, protocol.WorkspaceToolsRafiki,
		newFakeBinder(&fakeExec{id: "e-1"}, &fakeExec{id: "e-2"}))
	cs := mcpConnect(t, face.getServer(req))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "Read",
		Arguments: map[string]any{"file_path": "/work/x"},
	})
	ck.Require().NoError(err, "CallTool")
	ck.True(res.IsError, "an ambiguous workspace call must be reported as an error: %v", res)
	text := mcpErrorText(t, res)
	ck.StrContains(text, "this agent can reach 2 executors (e-1, e-2); pass executor:", "ambiguity diagnostic")
}

// mcpErrorText concatenates the text blocks of a FAILING CallTool result —
// mcpCallText (mcp_face_test.go) deliberately fatals on one.
func mcpErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
