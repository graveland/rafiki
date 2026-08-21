package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// What a child is told about its machine comes from the ROW.
//
// The executor answers Provision with isolation "none" for every workspace: it
// does not know whether it is running in a container, and this design forbids
// it from finding out. A daemon that believed that answer told every child it
// was unsandboxed, and BuildSystemPrompt drops the whole "Your machine" block
// when Isolation == "none" — so the warning vanished for precisely the workers
// that needed it, with every test still green.
func TestTheChildIsToldWhatTheRowSaysNotWhatTheExecutorClaims(t *testing.T) {
	wi := workspaceInfoFromRow(executors.Executor{
		ID: "exec-1", DisplayName: "ci-runner-2",
		Isolation: "container", WorkspaceMode: "ephemeral",
		Roots: []string{"/work", "/repo"},
	})

	if wi.Isolation != "container" {
		t.Fatalf("isolation = %q; a container executor's child must be told it is in a container", wi.Isolation)
	}
	if wi.WorkspaceMode != "ephemeral" {
		t.Errorf("workspace mode = %q, want ephemeral", wi.WorkspaceMode)
	}

	// The end-to-end property, not just the struct: the block must actually
	// reach the prompt. This is the assertion that would have failed.
	wi.ExecutorName = "ci-runner-2"
	got := fundi.BuildSystemPrompt(fundi.SysPromptConfig{
		Base: "base.", Cwd: "/work", ModelID: "m", Workspace: wi,
	})
	if !strings.Contains(got, "Your machine") {
		t.Fatalf("a container-isolated child got no workspace block:\n%s", got)
	}
	for _, want := range []string{"ci-runner-2", "container", "ephemeral", "/work", "/repo"} {
		if !strings.Contains(got, want) {
			t.Errorf("workspace block missing %q:\n%s", want, got)
		}
	}
}

// An unset workspace_mode on the row resolves to pinned, never to ephemeral.
//
// The same helper writes the rafiki/workspace-mode label, which decides whether
// losing an executor FAILS a child or moves it. Defaulting the other way — as
// HandleExecutorLost once did — reschedules children onto machines no operator
// ever marked interchangeable.
func TestAnUnsetWorkspaceModeIsPinned(t *testing.T) {
	if got := workspaceModeOrPinned(""); got != "pinned" {
		t.Fatalf("workspaceModeOrPinned(%q) = %q; an unknown mode must not be treated as disposable", "", got)
	}
	if got := workspaceModeOrPinned("ephemeral"); got != "ephemeral" {
		t.Fatalf("workspaceModeOrPinned(ephemeral) = %q", got)
	}
}

// The requested workspace mode must EXCLUDE executors whose row does not offer
// it. This was enforced by Provision on the executor, from a flag; the executor
// stopped declaring anything about itself and nothing replaced the check, so an
// inherited "ephemeral" landed happily on a pinned machine.
func TestWorkspaceModeNarrowsSelection(t *testing.T) {
	pinned := ex("exec-pinned", map[string]string{"env": "home"}, "")
	ephemeral := ex("exec-ephemeral", map[string]string{"env": "home"}, "")
	ephemeral.Executor.WorkspaceMode = "ephemeral"

	c := selectFixture(t, "env=home", pinned, ephemeral)
	req := protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "env=home"}

	req.WorkspaceMode = "ephemeral"
	chosen, err := c.chooseExecutor(req, "")
	if err != nil {
		t.Fatalf("an ephemeral request found no executor though one offers it: %v", err)
	}
	if chosen.ID != "exec-ephemeral" {
		t.Fatalf("placed on %s; only exec-ephemeral offers workspace_mode=ephemeral", chosen.ID)
	}

	// And with no ephemeral executor live, the spawn is REFUSED rather than
	// quietly downgraded to a pinned machine.
	c = selectFixture(t, "env=home", pinned)
	_, err = c.chooseExecutor(req, "")
	if err == nil {
		t.Fatal("an ephemeral request was satisfied by a pinned executor; the grant widened silently")
	}
	if !strings.Contains(err.Error(), "workspace_mode") {
		t.Errorf("the refusal does not name the mode that excluded every candidate: %v", err)
	}
}

// An executor's own Describe must not decide where other people's children run.
func TestSelectionIgnoresTheSelfReportedWorkspaceMode(t *testing.T) {
	liar := ex("exec-liar", map[string]string{"env": "home"}, "")
	liar.Describe = describeClaiming("ephemeral")

	c := selectFixture(t, "env=home", liar)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home", WorkspaceMode: "ephemeral",
	}, "")
	if err == nil {
		t.Fatal("an executor whose ROW says pinned attracted an ephemeral child by claiming ephemeral in Describe")
	}
}

func describeClaiming(mode string) *executorpb.DescribeResponse {
	return &executorpb.DescribeResponse{WorkspaceMode: mode}
}
