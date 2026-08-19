package tools

import (
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

// ls is a workspace tool the executor can serve today: LsBlueprint.Materialize
// needs only Cwd and OutputPolicy, both of which the executor's toolOptsFor
// supplies. The lsp_* family is deliberately absent until the executor hosts
// the LSP manager — forwarding them now would reach a registry that answers
// "unknown tool".
func TestRoutingLists(t *testing.T) {
	wantLocal := []string{"bash", "edit", "glob", "grep", "ls", "read", "write"}
	if got := ExecutorLocalTools(); !slices.Equal(got, wantLocal) {
		t.Errorf("ExecutorLocalTools() = %v, want %v", got, wantLocal)
	}

	wantRouted := []string{"bash", "bash_kill", "bash_output", "bash_start", "edit", "glob", "grep", "ls", "read", "write"}
	if got := RoutedToExecutor(); !slices.Equal(got, wantRouted) {
		t.Errorf("RoutedToExecutor() = %v, want %v", got, wantRouted)
	}

	for _, name := range RoutedToExecutor() {
		if len(name) > 4 && name[:4] == "lsp_" {
			t.Errorf("%q is routed to the executor, which cannot serve it yet", name)
		}
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
// FileTracker and OutputPolicy and nothing else — notably no LSP and no Tasks.
func TestExecutorLocalToolsAllMaterializeUnderExecutorOpts(t *testing.T) {
	opts := ToolOpts{
		Cwd:         t.TempDir(),
		FileTracker: NewFileTracker(),
	}
	got := registryNames(DefaultBlueprint.MaterializeOnly(opts, ExecutorLocalTools()))
	for _, name := range ExecutorLocalTools() {
		if !got[name] {
			t.Errorf("%q is in ExecutorLocalTools but declined to materialize under an executor's ToolOpts", name)
		}
	}
}
