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

// TestLocalForkRefusedReadsStoredConfinement pins the local-fork guard's sources:
// a non-empty executor grant anywhere in the stored lineage (the child's own
// selector on a resume, or the parent's on a fresh spawn), and sandbox
// ownership — the two facts that make a local fork on the daemon host an escape.
// An unconfined child is left alone.
func TestLocalForkRefusedReadsStoredConfinement(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	ctx := context.Background()

	// Unconfined: no grant anywhere, no sandbox.
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "", owner.UserID),
		"an unconfined child may fork locally")

	// The parent's stored grant (a fresh spawn: the child's own row does not exist).
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "p_conf", Status: protocol.StatusIdle, StartedAt: time.Now(), ExecutorSelector: "env=ci",
	})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_new", "p_conf", owner.UserID),
		"a parent's stored grant must confine its child")

	// The child's OWN stored grant (a resume/recovery: the row exists).
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_conf", Status: protocol.StatusIdle, StartedAt: time.Now(), ExecutorSelector: "machine=sbx-1",
	})
	ck.Error(env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_conf", "", owner.UserID),
		"a resumed child's own stored grant must confine it")

	// Sandbox ownership with NO stored selector: the belt-and-braces arm.
	env.ctrl.st.Insert(&childstore.Session{ChildID: "c_sbx", Status: protocol.StatusIdle, StartedAt: time.Now()})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c_sbx", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})
	err := env.ctrl.localForkRefused(ctx, protocol.KindScript, "c_sbx", "", owner.UserID)
	ck.Error(err, "sandbox ownership must confine a local fork even with no stored selector")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text")
	ck.StrContains(err.Error(), "sandbox", "refusal names the sandbox")
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

	_, err := env.ctrl.scriptRunner(protocol.SpawnRequest{
		Kind:          protocol.KindScript,
		ParentChildID: "parent",
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
		Kind:   protocol.KindScript,
		Script: &protocol.ScriptSpec{Repo: "local", Script: "driver"},
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

	_, err := env.ctrl.claudeRunner(protocol.SpawnRequest{
		Kind:          protocol.KindClaude,
		ParentChildID: "parent",
	}, "c_new", "", owner.UserID, nil)
	ck.Error(err, "a confined child's claude spawn must not fork locally")
	ck.StrContains(err.Error(), "confined to the executor plane", "refusal text: %v", err)

	// The unconfined child is unchanged: (nil, nil) means "take the local path".
	runner, err := env.ctrl.claudeRunner(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c_new2", "", owner.UserID, nil)
	ck.NoError(err, "an unconfined claude child keeps its local fallback")
	ck.True(runner == nil, "the local path is signalled by a nil runner")
}

// TestLocalForkRefusedWithAbsentParentAllowsResume pins the F1 regression: a
// child whose OWN row is live and unconfined but whose parent's row is gone
// (Controller.Close deletes it — controller.go's c.st.Delete(childID)) must
// still be allowed a local fork. The guard used to walk the whole lineage, and
// a missing ancestor made lineageChain return the misleading "lineage chain for
// X exceeds 64 links" error — refusing an ordinary resume of an exited,
// unconfined claude child. The fix is in the guard alone: when the child's own
// row exists it reads the child's own selector and never consults lineageChain,
// whose fail-closed error the executor-selection path still relies on.
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

	// The guard: the child's own row is present, so it reads its own selector
	// (empty) and does not walk the absent parent. Allowed.
	ck.NoError(env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_exit", "p_gone", owner.UserID),
		"a live, unconfined child with an absent parent may fork locally")

	// A child with a live row and a NON-EMPTY own selector is still refused,
	// even with an absent parent.
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_conf_exit", Status: protocol.StatusExited, StartedAt: time.Now(),
		ExecutorSelector: "machine=sbx-1",
		Labels:           map[string]string{childstore.LabelParent: "p_gone"},
	})
	err := env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_conf_exit", "p_gone", owner.UserID)
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
	err = env.ctrl.localForkRefused(ctx, protocol.KindClaude, "c_sbx_exit", "p_gone", owner.UserID)
	ck.Error(err, "a child owning a sandbox must still be refused")
	ck.StrContains(err.Error(), "sandbox", "refusal names the sandbox")
}
