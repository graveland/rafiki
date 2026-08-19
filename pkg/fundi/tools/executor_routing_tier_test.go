package tools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// Every registered blueprint must appear in the tier table, and the table must
// name nothing that is not registered. This is the test that would have caught
// `ls`: it was a real tool from the day it shipped and was never added to the
// routing list, so with an executor configured it listed directories in the
// daemon's process against the daemon's filesystem. A hand-written slice
// cannot notice a tool it was never told about.
func TestEveryBlueprintHasATier(t *testing.T) {
	registered := map[string]bool{}
	for _, bp := range DefaultBlueprint.All() {
		registered[bp.Name()] = true
	}

	for name := range registered {
		if _, ok := TierOf(name); !ok {
			t.Errorf("tool %q is registered but has no tier — add it to tierByTool in executor_routing.go", name)
		}
	}
	for name := range tierByTool {
		if !registered[name] {
			t.Errorf("tierByTool names %q, which no blueprint registers — stale entry", name)
		}
	}
}

// The six historical names plus ls and the lsp_* family are the workspace tier.
// Pinned explicitly rather than derived, so a change to the tier of any one of
// them is a deliberate edit to this list and not a silent side effect.
func TestWorkspaceTierMembership(t *testing.T) {
	want := []string{
		"bash", "bash_kill", "bash_output", "bash_start",
		"edit", "glob", "grep", "ls",
		"lsp_call_hierarchy", "lsp_definition", "lsp_diagnostics",
		"lsp_references", "lsp_rename", "lsp_restart", "lsp_symbols",
		"read", "write",
	}
	got := WorkspaceTools()
	if !slices.Equal(got, want) {
		t.Errorf("WorkspaceTools() = %v\nwant %v", got, want)
	}
}

// ls and the lsp_* family are workspace tools the executor can serve today:
// LsBlueprint.Materialize needs only Cwd and OutputPolicy, and the LSP tools
// need only the manager newLSPManager builds from the executor's own lsp.json
// or PATH, both of which the executor's toolOptsFor supplies.
func TestRoutingLists(t *testing.T) {
	wantLocal := []string{
		"bash", "edit", "glob", "grep", "ls",
		"lsp_call_hierarchy", "lsp_definition", "lsp_diagnostics",
		"lsp_references", "lsp_rename", "lsp_restart", "lsp_symbols",
		"read", "write",
	}
	if got := ExecutorLocalTools(); !slices.Equal(got, wantLocal) {
		t.Errorf("ExecutorLocalTools() = %v, want %v", got, wantLocal)
	}

	wantRouted := []string{
		"bash", "bash_kill", "bash_output", "bash_start",
		"edit", "glob", "grep", "ls",
		"lsp_call_hierarchy", "lsp_definition", "lsp_diagnostics",
		"lsp_references", "lsp_rename", "lsp_restart", "lsp_symbols",
		"read", "write",
	}
	if got := RoutedToExecutor(); !slices.Equal(got, wantRouted) {
		t.Errorf("RoutedToExecutor() = %v, want %v", got, wantRouted)
	}
}

// registryNames reads the set of tool names a Registry advertises. Registry has
// no lookup-by-name accessor; Definitions() is the supported way in, and is
// what mcp_test.go already uses.
func registryNames(r *Registry) map[string]bool {
	names := map[string]bool{}
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}
	return names
}

// A tool named in ExecutorLocalTools must actually materialize under the opts
// an executor process supplies, or routing a call to it reaches a registry
// that answers "unknown tool". The executor's toolOptsFor sets Cwd, RTK,
// FileTracker and OutputPolicy, and NewServer adds the LSP client its manager
// produced (and nothing else — notably no Tasks), so these opts carry one too.
func TestExecutorLocalToolsAllMaterializeUnderExecutorOpts(t *testing.T) {
	opts := ToolOpts{
		Cwd:         t.TempDir(),
		FileTracker: NewFileTracker(),
		LSP:         &fakeLSPClient{},
	}
	got := registryNames(DefaultBlueprint.MaterializeOnly(opts, ExecutorLocalTools()))
	for _, name := range ExecutorLocalTools() {
		if !got[name] {
			t.Errorf("%q is in ExecutorLocalTools but declined to materialize under an executor's ToolOpts", name)
		}
	}
}

// With an executor configured the LSP tools must be PRESENT — they are proxied
// to the executor, which runs the language servers against the files it holds.
// They decline only when there is neither a local LSP client nor an executor.
func TestLSPToolsMaterializeWithAnExecutor(t *testing.T) {
	lspNames := []string{
		"lsp_call_hierarchy", "lsp_definition", "lsp_diagnostics",
		"lsp_references", "lsp_rename", "lsp_restart", "lsp_symbols",
	}

	withExecutorOnly := ToolOpts{Cwd: t.TempDir(), Executor: stubExecutorClient{}}
	got := registryNames(DefaultBlueprint.MaterializeOnly(withExecutorOnly, lspNames))
	for _, name := range lspNames {
		if !got[name] {
			t.Errorf("%q declined with an executor configured; it should be proxied to it", name)
		}
	}

	neither := ToolOpts{Cwd: t.TempDir()}
	got2 := registryNames(DefaultBlueprint.MaterializeOnly(neither, lspNames))
	for _, name := range lspNames {
		if got2[name] {
			t.Errorf("%q materialized with no LSP client and no executor", name)
		}
	}
}

// stubExecutorClient satisfies ExecutorClient so a ToolOpts can carry a
// non-nil Executor. No method is ever called: these tests assert on which
// tools materialize, never on what they do.
type stubExecutorClient struct{}

func (stubExecutorClient) Execute(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}
func (stubExecutorClient) StartJob(context.Context, string) (string, error) { return "", nil }
func (stubExecutorClient) JobOutput(context.Context, string, int64) (JobSnapshot, error) {
	return JobSnapshot{}, nil
}
func (stubExecutorClient) KillJob(context.Context, string) error { return nil }
func (stubExecutorClient) Ping(context.Context) error            { return nil }
