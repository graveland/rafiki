package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// insertWithSet stores a child through the production setter, so the set the
// guard reads is the one a spawn persists rather than a hand-built field.
func insertWithSet(t *testing.T, ctrl *Controller, id, selector string, set []string) {
	t.Helper()
	ctrl.st.Insert(&childstore.Session{
		ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(), ExecutorSelector: selector,
	})
	if set != nil {
		if err := ctrl.st.SetExecutorSet(id, set); err != nil {
			t.Fatalf("set executor set on %s: %v", id, err)
		}
	}
}

// TestLocalForkRefusedReadsStoredConfinement pins the local-fork guard's
// sources for a child whose own row exists: its executor set (ids confine,
// "all" does not, a nil set resolves through the legacy selector) and sandbox
// ownership, the two facts that make a local fork on the daemon host an escape.
func TestLocalForkRefusedReadsStoredConfinement(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	ctx := context.Background()

	// Unconfined: no row, no parent, no sandbox.
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "", owner.UserID, []string{childstore.ExecutorSetAll}),
		"an unconfined child may fork locally")

	insertWithSet(t, env.ctrl, "c_set", "", []string{"A"})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_set", "", owner.UserID, nil),
		"a resumed child confined to a set must be refused")

	insertWithSet(t, env.ctrl, "c_empty", "", []string{})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_empty", "", owner.UserID, nil),
		"a child whose set is empty addresses nothing and must be refused")

	insertWithSet(t, env.ctrl, "c_all", "", []string{childstore.ExecutorSetAll})
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_all", "", owner.UserID, nil),
		"a child on all executors may fork locally")

	insertWithSet(t, env.ctrl, "c_legacy", "machine=sbx-1", nil)
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_legacy", "", owner.UserID, nil),
		"a legacy row with a selector must be refused")

	insertWithSet(t, env.ctrl, "c_legacy_open", "", nil)
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_legacy_open", "", owner.UserID, nil),
		"a legacy row with no selector addressed every executor")

	// The child's own row wins over its parent's: an unconfined own row is not
	// refused because a parent is confined (the subset relation makes the
	// child's set the whole answer once it exists).
	insertWithSet(t, env.ctrl, "p_conf", "", []string{"A"})
	insertWithSet(t, env.ctrl, "c_own_all", "", []string{childstore.ExecutorSetAll})
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_own_all", "p_conf", owner.UserID, nil),
		"the child's own row decides when it exists")

	// Sandbox ownership with NO stored set: the belt-and-braces arm, on both
	// an all set and a legacy empty selector.
	insertWithSet(t, env.ctrl, "c_sbx", "", nil)
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c_sbx", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})
	err := env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_sbx", "", owner.UserID, nil)
	ck.Error(err, "sandbox ownership must confine a local fork even with no stored set")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text")
	ck.StrContains(err.Error(), "sandbox", "refusal names the sandbox")

	insertWithSet(t, env.ctrl, "c_sbx_all", "", []string{childstore.ExecutorSetAll})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-2", OwnerUserID: owner.UserID, OwnerChild: "c_sbx_all", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_sbx_all", "", owner.UserID, nil),
		"sandbox ownership confines a child whose set is all")
}

// TestLocalForkRefusedReadsTheResolvedSetForAFreshSpawn: with no row for the
// child yet, the set resolveSpawnExecutorSet fixed on the request is the
// confinement, whatever the parent's set is: a finite child set under an all
// (or absent) parent is still confined.
func TestLocalForkRefusedReadsTheResolvedSetForAFreshSpawn(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	ctx := context.Background()
	insertWithSet(t, env.ctrl, "p_all", "", []string{childstore.ExecutorSetAll})

	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "p_all", owner.UserID, []string{childstore.ExecutorSetAll}),
		"an all child may fork locally")
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "p_all", owner.UserID, []string{"A"}),
		"a finite child set under an all parent confines the fresh child")
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "", owner.UserID, []string{"A"}),
		"a finite child set at the top level confines the fresh child")
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "", owner.UserID, []string{}),
		"an empty child set addresses nothing")
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "p_all", owner.UserID, nil),
		"a fresh child whose set was never resolved fails closed")

	// A sandbox owner under an unconfined parent is still refused.
	insertWithSet(t, env.ctrl, "p_sbx", "", []string{childstore.ExecutorSetAll})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-p", OwnerUserID: owner.UserID, OwnerChild: "p_sbx", Scope: protocol.ScopeSubtree,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "p_sbx", owner.UserID, []string{childstore.ExecutorSetAll}),
		"a subtree sandbox the parent holds confines a fresh child")
}

// TestScriptRunnerRefusesLocalForkForAConfinedChild: with no executor able to
// host a script child, a child whose parent carries an executor grant is
// REFUSED the local fork rather than forked on the daemon's own host. This is
// the same refusal checkKindNarrowing makes at Spawn, made at the fork.
func TestScriptRunnerRefusesLocalForkForAConfinedChild(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "parent", Status: protocol.StatusIdle, StartedAt: time.Now(), ExecutorSelector: "machine=sbx-1",
	})
	ck.NoError(env.ctrl.st.SetExecutorSet("parent", []string{"A"}), "confine the parent")

	_, err := env.ctrl.scriptRunner(protocol.SpawnRequest{
		Kind:          protocol.KindScript,
		ParentChildID: "parent",
		Executors:     []string{"A"},
		Script:        &protocol.ScriptSpec{Repo: "local", Script: "driver"},
	}, "c_new", "", owner.UserID)
	ck.Error(err, "a confined child's script spawn must not fork locally")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text: %v", err)
}

// TestScriptRunnerLocalForkUnconfinedUnchanged: an unconfined child still
// reaches the local runner — here it fails on the missing proxy face, never on
// confinement. The guard must not turn every local script child into a refusal.
func TestScriptRunnerLocalForkUnconfinedUnchanged(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()

	_, err := env.ctrl.scriptRunner(protocol.SpawnRequest{
		Kind:      protocol.KindScript,
		Executors: []string{childstore.ExecutorSetAll},
		Script:    &protocol.ScriptSpec{Repo: "local", Script: "driver"},
	}, "c_new", "", owner.UserID)
	ck.Error(err, "with no proxy face the local runner still refuses — for the proxy, not confinement")
	ck.StrContains(err.Error(), "proxy face", "the unconfined child reaches the local runner")
	ck.False(strings.Contains(err.Error(), "confined"),
		"an unconfined child must not be refused for confinement: %v", err)
}

// TestClaudeRunnerRefusesLocalForkForAConfinedChild: the same rule for claude's
// local-subprocess fallback (the reviewer's same-class finding).
func TestClaudeRunnerRefusesLocalForkForAConfinedChild(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "parent", Status: protocol.StatusIdle, StartedAt: time.Now(), ExecutorSelector: "env=ci",
	})
	ck.NoError(env.ctrl.st.SetExecutorSet("parent", []string{"A"}), "confine the parent")

	_, err := env.ctrl.claudeRunner(protocol.SpawnRequest{
		Kind:          protocol.KindClaude,
		ParentChildID: "parent",
		Executors:     []string{"A"},
	}, "c_new", "", owner.UserID, nil)
	ck.Error(err, "a confined child's claude spawn must not fork locally")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text: %v", err)

	// The unconfined child is unchanged: (nil, nil) means "take the local path".
	runner, err := env.ctrl.claudeRunner(protocol.SpawnRequest{Kind: protocol.KindClaude, Executors: []string{childstore.ExecutorSetAll}}, "c_new2", "", owner.UserID, nil)
	ck.NoError(err, "an unconfined claude child keeps its local fallback")
	ck.True(runner == nil, "the local path is signalled by a nil runner")
}

// TestLocalForkRefusedWithAbsentParentAllowsResume pins the F1 regression: a
// child whose OWN row is live and unconfined but whose parent's row is gone
// (Controller.Close deletes it — controller.go's c.st.Delete(childID)) must
// still be allowed a local fork: a missing ancestor must never refuse an
// ordinary resume. When the child's own row exists the guard reads the child's
// own set and never reads the parent.
func TestLocalForkRefusedWithAbsentParentAllowsResume(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	ctx := context.Background()

	// The child's own row is live; its parent ("p_gone") is absent from the store.
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_exit", Status: protocol.StatusExited, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "p_gone"},
	})

	// The guard: the child's own row is present, so it reads its own set
	// (legacy, empty) and does not walk the absent parent. Allowed.
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_exit", "p_gone", owner.UserID, nil),
		"a live, unconfined child with an absent parent may fork locally")

	// A child with a live row and a confining own set is still refused,
	// even with an absent parent.
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_conf_exit", Status: protocol.StatusExited, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "p_gone"},
	})
	ck.NoError(env.ctrl.st.SetExecutorSet("c_conf_exit", []string{"A"}), "confine the child")
	err := env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_conf_exit", "p_gone", owner.UserID, nil)
	ck.Error(err, "a confined child with an absent parent must still be refused")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text: %v", err)

	// A child with a live row and a sandbox it OWNS is still refused, even with
	// an absent parent and an empty selector.
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_sbx_exit", Status: protocol.StatusExited, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "p_gone"},
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-own", OwnerUserID: owner.UserID, OwnerChild: "c_sbx_exit", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})
	err = env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_sbx_exit", "p_gone", owner.UserID, nil)
	ck.Error(err, "a child owning a sandbox must still be refused")
	ck.StrContains(err.Error(), "sandbox", "refusal names the sandbox")
}
