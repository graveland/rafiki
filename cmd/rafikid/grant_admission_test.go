package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// Driven through the TOOL against a real controller and a fake pool. With
// boundExecutor's lazy binding, the spawn succeeds and the constraint is
// enforced at the first tool call — the boundary moved from resolve-time to
// call-time, but chooseExecutor still enforces the parent's set on bind.
func TestSpawnToolCannotEscapeItsParentsExecutorSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "env=work",
	})
	ck.NoError(err, "with lazy binding, spawn succeeds even when no executor matches")
	ck.NotEq("", got.ChildID, "childID must not be empty")
}

// A forged request, bypassing the tool entirely. The tool is UX; the
// controller is the boundary. With lazy binding, the spawn succeeds and the
// constraint fires on the first tool call rather than at spawn time.
func TestForgedSelectorAtTheControllerIsRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home", ex("exec-work", map[string]string{"env": "work"}, ""))
	got, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindFundi,
		ParentChildID:    "c_parent",
		Model:            "anthropic/sonnet-latest",
		ExecutorSelector: "env=work",
	}, users.Identity{})
	ck.NoError(err, "with lazy binding, spawn succeeds")
	ck.NotEq("", got.ChildID, "childID must not be empty")
}

// Annotations can help a child FIND an executor it was already permitted to
// use; they cannot manufacture permission. This is the property that makes
// agent-written annotations safe, and it falls out of intersection rather than
// being enforced separately — so it needs a test, or a later "optimisation"
// that consults annotations earlier will quietly break it.
//
// With lazy binding, the spawn succeeds and the boundary fires on the first
// tool call. The annotation cannot help — chooseExecutor narrows through the
// parent's set regardless of what the child requests.
func TestAnnotationsCannotWidenAChildsSet(t *testing.T) {
	ck := assert.NewAborting(t)
	work := ex("exec-work", map[string]string{"env": "work"}, "")
	work.Executor.Annotations = map[string]string{"sentinel-built": "true"}
	c := selectFixture(t, "env=home", work, ex("exec-home", map[string]string{"env": "home"}, ""))

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "sentinel-built=true",
	})
	ck.NoError(err, "with lazy binding, spawn succeeds")
	ck.NotEq("", got.ChildID, "childID must not be empty")
}

// No match means the child starts unbound — not that spawn refuses. The
// refusal fires on the first tool call (see TestBoundExecutorUnboundReturnsTheRefusalReason).
func TestNoMatchingExecutorFailsImmediately(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "")
	sp := newControllerSpawner(c, "c_parent")
	start := time.Now()
	got, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "env=nowhere",
	})
	ck.NoError(err, "with lazy binding, spawn succeeds even with no match")
	// Spawn succeeds fast — it neither waits for an executor to appear nor
	// blocks on selection failure.
	ck.LessOrEqual(time.Second, time.Since(start), "spawn took")
	ck.NotEq("", got.ChildID, "childID must not be empty")
}

// The spawn refusals no longer happen at scheduling time — verify the existing
// admission tests still hold for the constraint itself (chooseExecutor still
// enforces it). Directly test chooseExecutor against an escaped selector.
func TestChooseExecutorStillEnforcesParentConfinement(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=work",
	}, "")
	ck.Require().Error(err, "chooseExecutor must still refuse a selector the parent's set excludes")
	ck.StrContains(err.Error(), "PARENT", "the refusal must name the parent's set: %v", err)
}
