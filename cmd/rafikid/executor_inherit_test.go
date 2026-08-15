package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home"}, ""),
		ex("exec-work", map[string]string{"env": "work"}, ""),
	)

	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_parent"})

	if req.ExecutorSelector == "" {
		t.Fatal("the child inherited no selector; it and its whole subtree are unconfined")
	}
	if req.ExecutorSelector != "env=home" {
		t.Fatalf("selector = %q, want the parent's env=home", req.ExecutorSelector)
	}

	// And the inherited selector must actually CONFINE, not merely be stored.
	chosen, err := c.chooseExecutor(req)
	if err != nil {
		t.Fatalf("the child was not placed on an executor at all: %v", err)
	}
	if chosen.ID != "exec-home" {
		t.Fatalf("child placed on %s; the parent could only reach exec-home", chosen.ID)
	}
}

// The escape driven through Controller.Spawn itself, so that removing the
// inheritance call from Spawn — rather than from inheritExecutorGrant — is
// also caught. The spawn is expected to FAIL: no live executor satisfies the
// inherited selector, and explainNoMatch names it. Without inheritance the
// selector is empty, resolveExecutor answers (nil, nil), and the spawn gets
// as far as trying to start a process.
func TestSpawnAppliesTheInheritedSelector(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
	)
	c.stateDir = t.TempDir()

	_, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "c_parent",
		// No ExecutorSelector.
	})
	if err == nil {
		t.Fatal("the spawn was admitted despite no executor satisfying the parent's grant")
	}
	if !strings.Contains(err.Error(), "env=home") {
		t.Fatalf("the spawn did not apply the inherited selector; refusal was: %v", err)
	}
}

// Today's correct default, which the fix must not disturb: a top-level agent
// has no parent to inherit from and keeps running its tools in-process.
func TestTopLevelSpawnWithNoSelectorStaysLocal(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))

	req := c.inheritExecutorGrant(protocol.SpawnRequest{}) // no ParentChildID
	if req.ExecutorSelector != "" {
		t.Fatalf("a top-level spawn has nothing to inherit; got %q", req.ExecutorSelector)
	}

	cl, err := c.resolveExecutor(req)
	if err != nil {
		t.Fatalf("a top-level spawn with no selector must be permitted: %v", err)
	}
	if cl != nil {
		t.Fatal("a top-level spawn with no selector runs in-process; nil client means exactly that")
	}
}

// Inheritance is a floor, not an override. A child that names its own selector
// keeps it — and is still intersected with its parent's set, which is what
// stops the named selector from being a widening.
func TestExplicitSelectorIsNotOverwrittenByInheritance(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home", "gpu": "yes"}, ""),
	)

	req := c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "gpu=yes",
	})
	if req.ExecutorSelector != "gpu=yes" {
		t.Fatalf("selector = %q, want the child's own gpu=yes", req.ExecutorSelector)
	}

	// Still narrowed by the parent's stored selector, via effectiveExecutorSet.
	c2 := selectFixture(t, "env=home",
		ex("exec-other", map[string]string{"env": "work", "gpu": "yes"}, ""),
	)
	if _, err := c2.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "gpu=yes",
	}); err == nil {
		t.Fatal("naming a selector must not escape the parent's set")
	}
}

// The two halves of the grant are independent. A parent confined to ephemeral
// workspaces whose child inherits the selector but silently defaults to
// "pinned" has been handed a wider grant than its parent's through the back
// door.
func TestWorkspaceModeIsInheritedIndependentlyOfTheSelector(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.WorkspaceMode = "ephemeral" })

	// Silent on both.
	req := c.inheritExecutorGrant(protocol.SpawnRequest{ParentChildID: "c_parent"})
	if req.WorkspaceMode != "ephemeral" {
		t.Errorf("workspace mode = %q; a half-inherited grant is a widened grant", req.WorkspaceMode)
	}

	// Names a selector, silent on the mode: the mode must still be inherited.
	req = c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home",
	})
	if req.WorkspaceMode != "ephemeral" {
		t.Errorf("workspace mode = %q; naming a selector must not silently widen the mode", req.WorkspaceMode)
	}

	// Names its own mode: kept.
	req = c.inheritExecutorGrant(protocol.SpawnRequest{
		ParentChildID: "c_parent", WorkspaceMode: "pinned",
	})
	if req.WorkspaceMode != "pinned" {
		t.Errorf("an explicit mode must be kept, got %q", req.WorkspaceMode)
	}
}

// The grant is read from the STORE, never from the request — the same rule
// the limits checks follow. A caller that could name its own parent's grant
// could widen it.
func TestInheritanceReadsTheStoreNotTheRequest(t *testing.T) {
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
	if req.ExecutorSelector != "env=home" {
		t.Fatalf("selector = %q, want the stored env=home", req.ExecutorSelector)
	}
	chosen, err := c.chooseExecutor(req)
	if err != nil {
		t.Fatalf("chooseExecutor: %v", err)
	}
	if chosen.ID != "exec-home" {
		t.Fatalf("a third-generation child reached %s, outside its lineage's set", chosen.ID)
	}
}
