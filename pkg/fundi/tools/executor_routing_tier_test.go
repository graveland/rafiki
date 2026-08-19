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

// ExecutorLocalTools and RoutedToExecutor must not change in this commit.
// Their contents are what an executor process actually serves today; widening
// either before the executor can run the new tool routes a call to a registry
// that will answer "unknown tool".
func TestRoutingListsAreUnchanged(t *testing.T) {
	wantLocal := []string{"bash", "edit", "glob", "grep", "read", "write"}
	if got := ExecutorLocalTools(); !slices.Equal(got, wantLocal) {
		t.Errorf("ExecutorLocalTools() = %v, want %v", got, wantLocal)
	}

	wantRouted := []string{"bash", "bash_kill", "bash_output", "bash_start", "edit", "glob", "grep", "read", "write"}
	if got := RoutedToExecutor(); !slices.Equal(got, wantRouted) {
		t.Errorf("RoutedToExecutor() = %v, want %v", got, wantRouted)
	}
}
