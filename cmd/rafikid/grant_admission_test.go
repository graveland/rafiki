package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// Driven through the TOOL, against a real controller and a fake pool. The
// equivalent test at selectExecutor (plan-07b task 2) proves the set
// operation; this proves an agent cannot get past it.
func TestSpawnToolCannotEscapeItsParentsExecutorSet(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	sp := newControllerSpawner(c, "c_parent")
	_, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "env=work",
	})
	if err == nil {
		t.Fatal("the tool reached outside its parent's set")
	}
	if !strings.Contains(err.Error(), "exec-work") {
		t.Errorf("the refusal must name the executor it could not have: %v", err)
	}
}

// A forged request, bypassing the tool entirely. The tool is UX; the
// controller is the boundary.
func TestForgedSelectorAtTheControllerIsRefused(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-work", map[string]string{"env": "work"}, ""))
	_, err := c.Spawn(context.Background(), protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindFundi,
		ParentChildID:    "c_parent",
		Model:            "anthropic/sonnet-latest",
		ExecutorSelector: "env=work",
	}, users.Identity{})
	if err == nil {
		t.Fatal("a check that only ran in the tool would pass a test driven through the tool")
	}
}

// Annotations can help a child FIND an executor it was already permitted to
// use; they cannot manufacture permission. This is the property that makes
// agent-written annotations safe, and it falls out of intersection rather than
// being enforced separately — so it needs a test, or a later "optimisation"
// that consults annotations earlier will quietly break it.
func TestAnnotationsCannotWidenAChildsSet(t *testing.T) {
	work := ex("exec-work", map[string]string{"env": "work"}, "")
	work.Executor.Annotations = map[string]string{"sentinel-built": "true"}
	c := selectFixture(t, "env=home", work, ex("exec-home", map[string]string{"env": "home"}, ""))

	sp := newControllerSpawner(c, "c_parent")
	_, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "sentinel-built=true",
	})
	if err == nil {
		t.Fatal("an annotation let a child onto a machine its parent could not reach")
	}
}

// No match fails NOW. Queueing is opt-in only; silent queueing turns a typo in
// a selector into a hang nobody can diagnose.
func TestNoMatchingExecutorFailsImmediately(t *testing.T) {
	c := selectFixture(t, "")
	sp := newControllerSpawner(c, "c_parent")
	start := time.Now()
	_, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", ExecutorSelector: "env=nowhere",
	})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("spawn took %s; scheduling failure must be fast", d)
	}
}
