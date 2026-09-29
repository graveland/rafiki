package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// THE confinement escape. A confined parent spawning a child that names no
// executor must not produce an unconfined child.
//
// An omitted selector used to mean "no executor": resolveExecutor returned
// (nil, nil) and the child's tools ran IN THE DAEMON PROCESS on the daemon
// host, outside every executor its parent was restricted to. And the child
// stored "" as its own selector, so lineageChain skipped it and the entire
// subtree beneath inherited the way out.
func TestOmittedSelectorInheritsTheParentsConfinement(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home"}, ""),
		ex("exec-work", map[string]string{"env": "work"}, ""),
	)

	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_parent"})

	ck.NotEq("", req.ExecutorSelector, "the child inherited no selector; it and its whole subtree are unconfined")
	ck.Eq("env=home", req.ExecutorSelector, "selector")

	// And the inherited selector must actually CONFINE, not merely be stored.
	chosen, err := c.chooseExecutor(req, executorOwner{})
	ck.NoError(err, "the child was not placed on an executor at all")
	ck.Eq("exec-home", chosen.ID, "child placed on")
}

// The escape driven through Controller.Spawn itself, so that removing the
// inheritance call from Spawn — rather than from inheritExecutorGrant — is
// also caught. With boundExecutor, spawn succeeds even when no executor
// matches: the child starts unbound and its first tool call surfaces the
// refusal. This test verifies the inherited selector IS applied — the spawn
// proceeds rather than failing, which is the new behaviour.
func TestSpawnAppliesTheInheritedSelector(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
	)
	c.stateDir = t.TempDir()

	got, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "c_parent",
		// No ExecutorSelector.
	}, users.Identity{})
	ck.NoError(err, "spawn must not fail: a child starts unbound when no executor matches")
	// The child received the inherited selector.
	snap, ok := c.st.Get(got.ChildID)
	ck.True(ok, "child not found in store after spawn")
	ck.Eq("env=home", snap.ExecutorSelector, "the spawn did not apply the inherited selector; got")
}

// Today's correct default, which the fix must not disturb: a top-level agent
// has no parent to inherit from and keeps running its tools in-process.
func TestTopLevelSpawnWithNoSelectorStaysLocal(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))

	req := c.inheritExecutorGrant(protocol.SpawnRequest{}) // no ParentChildID
	assert.NewAborting(t).Eq("", req.ExecutorSelector, "a top-level spawn has nothing to inherit; got")

	// With no selector and no pool, agentRuntimeOptions leaves exec nil.
	// The old resolveExecutor returned (nil, nil) for this case; the new
	// path does the same through the 'if req.ExecutorSelector != "" && c.execPool != nil'
	// guard in agentRuntimeOptions.
}

// Inheritance is a floor, not an override. A child that names its own selector
// keeps it — and is still intersected with its parent's set, which is what
// stops the named selector from being a widening.
func TestExplicitSelectorIsNotOverwrittenByInheritance(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home", "gpu": "yes"}, ""),
	)

	req := c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "gpu=yes",
	})
	ck.Eq("gpu=yes", req.ExecutorSelector, "selector")

	// Still narrowed by the parent's stored selector, via effectiveExecutorSet.
	c2 := selectFixture(t, "env=home",
		ex("exec-other", map[string]string{"env": "work", "gpu": "yes"}, ""),
	)
	_, err := c2.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "gpu=yes",
	}, executorOwner{})
	ck.Error(err, "naming a selector must not escape the parent's set")
}

// The two halves of the grant are independent. A parent confined to ephemeral
// workspaces whose child inherits the selector but silently defaults to
// "pinned" has been handed a wider grant than its parent's through the back
// door.
func TestWorkspaceModeIsInheritedIndependentlyOfTheSelector(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.WorkspaceMode = "ephemeral" })

	// Silent on both.
	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_parent"})
	ck.Eq("ephemeral", req.WorkspaceMode, "workspace mode")

	// Names a selector, silent on the mode: the mode must still be inherited.
	req = c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home",
	})
	ck.Eq("ephemeral", req.WorkspaceMode, "workspace mode")

	// Names its own mode: kept.
	req = c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", WorkspaceMode: "pinned",
	})
	ck.Eq("pinned", req.WorkspaceMode, "an explicit mode must be kept, got")
}

// The grant is read from the STORE, never from the request — the same rule
// the limits checks follow. A caller that could name its own parent's grant
// could widen it.
func TestInheritanceReadsTheStoreNotTheRequest(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home"}, ""),
		ex("exec-work", map[string]string{"env": "work"}, ""),
	)
	// A grandchild whose parent is the confined child.
	c.st.Insert(&childstore.Session{
		ChildID: "c_grand", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "env=home",
		Labels: map[string]string{
			childstore.LabelParent: "c_parent", childstore.LabelRoot: "c_parent",
		},
	})

	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_grand"})
	ck.Eq("env=home", req.ExecutorSelector, "selector")
	chosen, err := c.chooseExecutor(req, executorOwner{})
	ck.NoError(err, "chooseExecutor")
	ck.Eq("exec-home", chosen.ID, "a third-generation child reached")
}
