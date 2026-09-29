package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	wi := workspaceInfoFromRow(executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "ci-runner-2"},
		Isolation: "container", WorkspaceMode: "ephemeral",
		Roots: []string{"/work", "/repo"},
	})

	c.Require().Eq("container", wi.Isolation, "isolation")
	c.Eq("ephemeral", wi.WorkspaceMode, "workspace mode")

	// The end-to-end property, not just the struct: the block must actually
	// reach the prompt. This is the assertion that would have failed.
	// The name is the `machine` label; agent_runtime reads it from the same row.
	wi.ExecutorName = "ci-runner-2"
	got := fundi.BuildSystemPrompt(fundi.SysPromptConfig{
		Base: "base.", Cwd: "/work", ModelID: "m", Workspace: wi,
	})
	c.Require().StrContains(got, "Your machine", "a container-isolated child got no workspace block:\n")
	for _, want := range []string{"ci-runner-2", "container", "ephemeral", "/work", "/repo"} {
		c.StrContains(got, want, "workspace block missing")
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
	assert.NewAborting(t).Eq("ephemeral", workspaceModeOrPinned("ephemeral"), "workspaceModeOrPinned(ephemeral) =")
}

// The requested workspace mode must EXCLUDE executors whose row does not offer
// it. This was enforced by Provision on the executor, from a flag; the executor
// stopped declaring anything about itself and nothing replaced the check, so an
// inherited "ephemeral" landed happily on a pinned machine.
func TestWorkspaceModeNarrowsSelection(t *testing.T) {
	ck := assert.NewCollecting(t)
	pinned := ex("exec-pinned", map[string]string{"env": "home"}, "")
	ephemeral := ex("exec-ephemeral", map[string]string{"env": "home"}, "")
	ephemeral.Executor.WorkspaceMode = "ephemeral"

	c := selectFixture(t, "env=home", pinned, ephemeral)
	req := protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "env=home"}

	req.WorkspaceMode = "ephemeral"
	chosen, err := c.chooseExecutor(req, executorOwner{})
	ck.Require().NoError(err, "an ephemeral request found no executor though one offers it")
	ck.Require().Eq("exec-ephemeral", chosen.ID, "placed on")

	// And with no ephemeral executor live, the spawn is REFUSED rather than
	// quietly downgraded to a pinned machine.
	c = selectFixture(t, "env=home", pinned)
	_, err = c.chooseExecutor(req, executorOwner{})
	ck.Require().Error(err, "an ephemeral request was satisfied by a pinned executor; the grant widened silently")
	ck.StrContains(err.Error(), "workspace_mode", "the refusal does not name the mode that excluded every candidate: %v", err)
}

// An executor's own Describe must not decide where other people's children run.
func TestSelectionIgnoresTheSelfReportedWorkspaceMode(t *testing.T) {
	liar := ex("exec-liar", map[string]string{"env": "home"}, "")
	liar.Describe = describeClaiming("ephemeral")

	c := selectFixture(t, "env=home", liar)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home", WorkspaceMode: "ephemeral",
	}, executorOwner{})
	assert.NewAborting(t).Error(err, "an executor whose ROW says pinned attracted an ephemeral child by claiming ephemeral in Describe")
}

func describeClaiming(mode string) *executorpb.DescribeResponse {
	return &executorpb.DescribeResponse{WorkspaceMode: mode}
}
