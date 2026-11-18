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

// Driven through Controller.Spawn itself, so that removing the set resolution
// from Spawn, rather than from resolveSpawnExecutorSet, is also caught. A child
// that names nothing gets a set that is a subset of its spawner's, stored on its
// row: the spawner's legacy env=home selector leaves exec-home and nothing else.
func TestSpawnStoresASetInsideTheSpawnersSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	c.stateDir = t.TempDir()

	got, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "c_parent",
	}, users.Identity{})
	ck.NoError(err)
	snap, ok := c.st.Get(got.ChildID)
	ck.True(ok, "child not found in store after spawn")
	ck.EqDeep([]string{"exec-home"}, snap.ExecutorSet, "the stored set")
	ck.Eq("", snap.ExecutorSelector, "the executor selector is no longer inherited")
}

// Today's correct default, which the fix must not disturb: a top-level agent
// has no parent to inherit from and keeps running its tools in-process.
func TestTopLevelSpawnWithNoSelectorStaysLocal(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))

	req := c.inheritExecutorGrant(protocol.SpawnRequest{}) // no ParentChildID
	assert.NewAborting(t).Eq("", req.ExecutorSelector, "a top-level spawn has nothing to inherit; got")

	// With no selector and no pool, spawnAgentRuntimeOptions leaves exec nil.
	// The old resolveExecutor returned (nil, nil) for this case; the new
	// path does the same through the 'if req.ExecutorSelector != "" && c.execPool != nil'
	// guard in spawnAgentRuntimeOptions.
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

// The workspace mode is read from the STORE, never from the request: a caller
// that could name its own parent's grant could widen it. The executor selector
// is not inherited at all; the set is resolved by resolveSpawnExecutorSet.
func TestInheritanceReadsTheStoreNotTheRequest(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	c.st.Insert(&childstore.Session{
		ChildID: "c_grand", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "env=home", WorkspaceMode: "ephemeral",
		Labels: map[string]string{
			childstore.LabelParent: "c_parent", childstore.LabelRoot: "c_parent",
		},
	})

	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_grand"})
	ck.Eq("ephemeral", req.WorkspaceMode, "workspace mode")
	ck.Eq("", req.ExecutorSelector, "the selector is not copied from the spawner")
}
