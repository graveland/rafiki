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

// Driven through the TOOL against a real controller and a fake pool. A selector
// outside the spawner's set matches nothing the spawner may address, so the
// spawn is refused before anything starts.
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
	ck.Error(err, "a selector outside the spawner's set must refuse the spawn")
	ck.StrContains(err.Error(), "no executor satisfies", "refusal text")
	ck.Eq("", got.ChildID, "no child may be created")
}

// A forged request, bypassing the tool entirely. The tool is UX; the
// controller is the boundary, and it refuses at spawn.
func TestForgedSelectorAtTheControllerIsRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home", ex("exec-work", map[string]string{"env": "work"}, ""))
	got, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindFundi,
		ParentChildID:    "c_parent",
		Model:            "anthropic/sonnet-latest",
		ExecutorSelector: "env=work",
	}, users.Identity{})
	ck.Error(err, "a forged selector outside the spawner's set must refuse the spawn")
	ck.Eq("", got.ChildID, "no child may be created")
}

// Annotations can help a child FIND an executor it was already permitted to
// use; they cannot manufacture permission. This is the property that makes
// agent-written annotations safe, and it falls out of intersection rather than
// being enforced separately — so it needs a test, or a later "optimisation"
// that consults annotations earlier will quietly break it.
//
// The annotation cannot help: the spawn narrows the spawner's set by what the
// child requests, and the annotated executor is outside that set.
func TestAnnotationsCannotWidenAChildsSet(t *testing.T) {
	ck := assert.NewAborting(t)
	work := ex("exec-work", map[string]string{"env": "work"}, "")
	work.Executor.Annotations = map[string]string{"sentinel-built": "true"}
	c := selectFixture(t, "env=home", work, ex("exec-home", map[string]string{"env": "home"}, ""))

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "sentinel-built=true",
	})
	ck.Error(err, "an annotation must not let a child past its spawner's set")
	ck.Eq("", got.ChildID, "no child may be created")
}

// A selector that matches nothing refuses the spawn at once, naming why.
func TestNoMatchingExecutorFailsImmediately(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "")
	sp := newControllerSpawner(c, "c_parent")
	start := time.Now()
	got, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "env=nowhere",
	})
	ck.Error(err, "a selector that matches nothing must refuse the spawn")
	ck.StrContains(err.Error(), "no executor satisfies", "refusal text")
	ck.Eq("", got.ChildID, "no child may be created")
	// The refusal is fast — it neither waits for an executor to appear nor
	// blocks on selection failure. This fixture wires a fake pool and no
	// executor store, so the waits a regression would have to introduce are
	// ones it adds itself; the bound is set well below the waits the real
	// design uses (execpool's join and health timeouts are 10s each, the park
	// timeout is 5 minutes) so it still fails on a genuine wait while leaving
	// room for an ordinary scheduling hiccup on a loaded host. 1s did not: a
	// loaded run took 1.066s and tripped it.
	ck.LessOrEqual(5*time.Second, time.Since(start), "spawn took")
}
