package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// sandboxEx is ex (executor_select_test.go) with the sandbox row label stamped
// on, so selection reads it as a sandbox row.
func sandboxEx(id, admits string, labels map[string]string) execpool.LiveExecutor {
	merged := map[string]string{}
	for k, v := range labels {
		merged[k] = v
	}
	merged[sandbox.RowLabelSandbox] = "1"
	return ex(id, merged, admits)
}

// childOwnedSandboxEx is a sandbox row that additionally carries the
// owner-child label, so it reads as a child-owned sandbox.
func childOwnedSandboxEx(id, ownerChild string, labels map[string]string) execpool.LiveExecutor {
	merged := map[string]string{}
	for k, v := range labels {
		merged[k] = v
	}
	merged[sandbox.RowLabelSandbox] = "1"
	merged[sandbox.RowLabelOwnerChild] = ownerChild
	return ex(id, merged, "")
}

// A sandbox is never an implicit choice: with neither a ref nor a selector the
// caller has named no machine, so selection must never fall back to a sandbox.
func TestSandboxSelectionEmptySelectorNeverPicksASandbox(t *testing.T) {
	ck := assert.NewAborting(t)

	// The sandbox is the ONLY row, so absent the rule it would be candidates[0]
	// and be picked.
	only := selectFixture(t, "", sandboxEx("aaa-sbx", "", map[string]string{"env": "home"}))
	_, err := only.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Error(err, "an empty selector must never implicitly pick a sandbox")

	// With an ordinary executor also present, the ordinary one wins.
	mixed := selectFixture(t, "",
		sandboxEx("aaa-sbx", "", map[string]string{"env": "home"}),
		ex("zzz-ordinary", map[string]string{"env": "home"}, ""),
	)
	chosen, err := mixed.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Require().NoError(err, "an ordinary executor is present and must be chosen")
	ck.Eq("zzz-ordinary", chosen.ID, "empty selector must pick the ordinary executor")
}

// A BROAD selector that matches both an ordinary executor and a sandbox prefers
// the ordinary one, even when the sandbox would otherwise sort first by ID.
func TestSandboxSelectionBroadSelectorPrefersOrdinaryExecutors(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		sandboxEx("aaa-sbx", "", map[string]string{"owner": "brent", "env": "home"}),
		ex("zzz-ordinary", map[string]string{"owner": "brent", "env": "home"}, ""),
	)
	chosen, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "owner=brent",
	}, executorOwner{Name: "brent"})
	ck.Require().NoError(err, "both rows match owner=brent")
	ck.Eq("zzz-ordinary", chosen.ID, "a broad selector must prefer an ordinary executor over a sandbox")
}

// A selector that NAMES a sandbox's machine still reaches it: the prohibition is
// on IMPLICIT choice, not on explicitly asking for one.
func TestSandboxSelectionExplicitMachineSelectorReachesANamedSandbox(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		sandboxEx("aaa-sbx", "", map[string]string{"machine": "greyshift"}),
		ex("zzz-ordinary", map[string]string{"machine": "silvershift"}, ""),
	)
	chosen, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "machine=greyshift",
	}, executorOwner{})
	ck.Require().NoError(err, "machine=greyshift names the sandbox explicitly")
	ck.Eq("aaa-sbx", chosen.ID, "an explicit machine selector must reach a named sandbox")
}

// A child-owned sandbox is never an ordinary candidate: not by implicit choice,
// not by a selector that matches it, and it never shadows an ordinary executor.
func TestSandboxSelectionChildOwnedNeverACandidate(t *testing.T) {
	ck := assert.NewAborting(t)

	// Alone: an empty selector is refused (the child-owned row is dropped).
	only := selectFixture(t, "", childOwnedSandboxEx("aaa-sbx", "c_parent", map[string]string{"env": "home"}))
	_, err := only.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Error(err, "a child-owned sandbox must never be an implicit candidate")

	// Alone: a broad selector that matches it is still refused.
	only2 := selectFixture(t, "", childOwnedSandboxEx("aaa-sbx", "c_parent", map[string]string{"owner": "brent"}))
	_, err = only2.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "owner=brent",
	}, executorOwner{Name: "brent"})
	ck.Error(err, "a child-owned sandbox must never be reached by a selector")

	// With an ordinary row present, the ordinary row wins.
	mixed := selectFixture(t, "",
		childOwnedSandboxEx("aaa-sbx", "c_parent", map[string]string{"owner": "brent"}),
		ex("zzz-ordinary", map[string]string{"owner": "brent"}, ""),
	)
	chosen, err := mixed.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "owner=brent",
	}, executorOwner{Name: "brent"})
	ck.Require().NoError(err, "the ordinary row must serve the spawn")
	ck.Eq("zzz-ordinary", chosen.ID, "a child-owned sandbox must not shadow the ordinary executor")
}

// An explicit ref naming a child-owned sandbox's machine label resolves to
// nothing: resolveRef reports no match because the row was never a candidate.
func TestSandboxSelectionChildOwnedNeverReachedByExplicitRef(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		childOwnedSandboxEx("sbx", "c_parent", map[string]string{"machine": "greyshift"}),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorRef: "greyshift",
	}, executorOwner{})
	ck.Error(err, "an explicit ref naming a child-owned sandbox's machine must be refused")
}

func TestSandboxSelectionNeverPicksASandboxWhitespaceOrEmptyKey(t *testing.T) {
	ck := assert.NewAborting(t)

	// (a) A whitespace-only selector is NOT an implicit selector: it fails
	// `== ""` but parses to the empty selector, so on a path that does not run
	// promoteBareExecutorRef it would match everything — including a sandbox-only
	// set. The implicit-sandbox drop must treat it as unnamed.
	ws := selectFixture(t, "", sandboxEx("aaa-sbx", "", map[string]string{"env": "home"}))
	_, err := ws.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: " ",
	}, executorOwner{})
	ck.Error(err, "a whitespace-only selector must not implicitly pick a sandbox")

	// (b) A row with the sandbox label key PRESENT but EMPTY is still a sandbox
	// (fail closed): alone it is refused, and with an ordinary row it does not
	// shadow it.
	only := selectFixture(t, "", ex("aaa-sbx", map[string]string{sandbox.RowLabelSandbox: ""}, ""))
	_, err = only.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Error(err, "a present-but-empty sandbox label must still exclude the row")

	mixed := selectFixture(t, "",
		ex("aaa-sbx", map[string]string{sandbox.RowLabelSandbox: "", "env": "home"}, ""),
		ex("zzz-ordinary", map[string]string{"env": "home"}, ""),
	)
	chosen, err := mixed.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Require().NoError(err, "the ordinary row must serve the spawn")
	ck.Eq("zzz-ordinary", chosen.ID, "an empty sandbox value must still be treated as a sandbox")
}

// Explicit-ref coverage beyond the machine-label path: a child-owned sandbox is
// also never reached when named by its raw ID ref.
func TestSandboxSelectionChildOwnedNeverReachedByIDRef(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		childOwnedSandboxEx("sbx-id", "c_parent", map[string]string{"env": "home"}),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorRef: "sbx-id",
	}, executorOwner{})
	ck.Error(err, "an explicit ref by row id naming a child-owned sandbox must be refused")
}

// Rule 1's LAUNCH half: an empty selector with a sandbox-only launch candidate
// set must not return the sandbox. Pinned here so that deleting the drop in
// chooseLaunchExecutor fails a test.
func TestSandboxSelectionEmptySelectorNeverPicksASandboxOnLaunch(t *testing.T) {
	ck := assert.NewAborting(t)

	// The sandbox is the ONLY launch-capable row, so absent the drop it would be
	// kept[0].
	only := selectFixture(t, "", exWithLaunch("aaa-sbx", map[string]string{
		sandbox.RowLabelSandbox: "1", "env": "home",
	}, "", "claude"))
	_, err := only.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	ck.Error(err, "an empty selector must never implicitly pick a sandbox for launch")

	// With an ordinary launch-capable executor present, the ordinary one wins.
	mixed := selectFixture(t, "",
		exWithLaunch("aaa-sbx", map[string]string{sandbox.RowLabelSandbox: "1", "env": "home"}, "", "claude"),
		exWithLaunch("zzz-ordinary", map[string]string{"env": "home"}, "", "claude"),
	)
	chosen, err := mixed.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	ck.Require().NoError(err, "an ordinary launch-capable executor is present")
	ck.Eq("zzz-ordinary", chosen.ID, "empty selector must pick the ordinary launch executor")
}

// Launch selection narrows through the same pipeline, so a child-owned sandbox
// that advertises the launch kind is still never reached.
func TestSandboxSelectionChildOwnedNeverReachedByLaunchSelection(t *testing.T) {
	le := exWithLaunch("sbx", map[string]string{
		sandbox.RowLabelSandbox:    "1",
		sandbox.RowLabelOwnerChild: "c_parent",
		"env":                      "home",
	}, "", "claude")
	c := selectFixture(t, "", le)
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	assert.NewAborting(t).Error(err, "launch selection must never reach a child-owned sandbox")
}

// The unchanged-behaviour guard: with no sandbox rows present the candidate set
// and its ordering are exactly what they were before this change.
func TestSandboxSelectionNoSandboxRowsIsANoOp(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		ex("aaa-durable", map[string]string{"env": "home"}, ""),
		ex("bbb-session", map[string]string{"env": "home", "kind": "session"}, ""),
		ex("ccc-durable", map[string]string{"env": "home"}, ""),
	)
	candidates, _, _, _, err := c.narrowedExecutorCandidates(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Require().NoError(err)
	// Pre-change ordering: durable before session, then by ID.
	ck.EqDiff([]string{"aaa-durable", "ccc-durable", "bbb-session"},
		ids(candidates), "with no sandbox rows, the candidate set and order must be unchanged")
}

// A row whose owner-child label key is PRESENT but EMPTY is still child-owned —
// fail closed, since the key is only ever written together with a value.
func TestSandboxSelectionEmptyOwnerChildValueStillExcluded(t *testing.T) {
	ck := assert.NewAborting(t)

	mixed := selectFixture(t, "",
		childOwnedSandboxEx("aaa-sbx", "", map[string]string{"owner": "brent"}),
		ex("zzz-ordinary", map[string]string{"owner": "brent"}, ""),
	)
	chosen, err := mixed.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "owner=brent",
	}, executorOwner{Name: "brent"})
	ck.Require().NoError(err, "the ordinary row must serve the spawn")
	ck.Eq("zzz-ordinary", chosen.ID, "an empty owner-child value must still be treated as child-owned")

	// Alone: refused even with an empty selector.
	only := selectFixture(t, "", childOwnedSandboxEx("aaa-sbx", "", map[string]string{"env": "home"}))
	_, err = only.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Error(err, "a present-but-empty owner-child label must still exclude the row")
}
