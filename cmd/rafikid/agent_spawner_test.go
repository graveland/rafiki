package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// spawnerFixture builds a Controller with a hand-populated childstore, which
// is all the adapter's authority checks read. No processes are started: the
// point is that a forged request is refused by STORED state, so the check must
// be observable without a live child.
//
//	c_root
//	 └── c_mine        (the caller)
//	      └── c_grandchild
//	c_stranger         (another top-level tree)
func spawnerFixture(t *testing.T) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	insert := func(id, parent, root string) {
		labels := map[string]string{}
		if parent != "" {
			labels[childstore.LabelParent] = parent
			labels[childstore.LabelRoot] = root
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Labels: labels,
			StartedAt: time.Now(), Kind: protocol.KindFundi,
		})
	}
	insert("c_root", "", "")
	insert("c_mine", "c_root", "c_root")
	insert("c_grandchild", "c_mine", "c_root")
	insert("c_stranger", "", "")
	return c
}

func TestSpawnerListReturnsOnlyDescendants(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	kids, err := sp.List(context.Background())
	c.Require().NoError(err)
	c.Require().False(len(kids) != 1 || kids[0].ChildID != "c_grandchild", "want only c_grandchild, got %+v", kids)
	c.Eq(1, kids[0].Depth, "want depth 1, got")
}

func TestSpawnerRefusesNonDescendantOnEveryVerb(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	ctx := context.Background()

	// A sibling tree, the caller's own parent, and the caller itself are all
	// non-descendants. A check that only rejected the first would pass a test
	// that only tried the first.
	for _, target := range []string{"c_stranger", "c_root", "c_mine", "c_nonexistent"} {
		if _, err := sp.View(ctx, target, 0); err == nil {
			t.Errorf("View(%s) must refuse", target)
		}
		c.Error(sp.Send(ctx, target, "hi"), "Send(%s) must refuse", target)
		c.Error(sp.Kill(ctx, target), "Kill(%s) must refuse", target)
	}
}

func TestSpawnerRefusalNamesTheTarget(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	err := sp.Send(context.Background(), "c_stranger", "hi")
	c.Require().Error(err, "want a refusal")
	// An error message is a prompt that is only paid when it is needed
	// (prompting.md). It must say which id was rejected and why.
	c.StrContains(err.Error(), "c_stranger", "refusal must name the target; got %v", err)
	c.StrContains(err.Error(), "descendant", "refusal must say why; got %v", err)
}

// The phase's ledger invariant, tested where it lives. A spawn that the
// controller refuses must write nothing to the ledger — no assignment, no
// row to roll back. This is asserted with a controller that refuses every
// spawn, so it holds for whatever refuses it: bad cwd today, a depth or cost
// ceiling once phase 05 lands.
func TestRefusedSpawnAssignsNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	c := spawnerFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	// Give the caller a conversation and a task to delegate.
	_ = c.st.Update("c_mine", func(s *childstore.Session) { s.SessionID = "conv-mine" })
	if _, err := store.Add(ctx, "conv-mine", "", []tasks.NewTask{{Content: "delegate me"}}); err != nil {
		t.Fatal(err)
	}

	sp := newControllerSpawner(c, "c_mine")
	// A cwd that does not exist is refused by Controller.Spawn's first check,
	// before anything is registered — but only for a kind the daemon itself
	// forks a subprocess for (claude). A fundi child's filesystem access
	// goes through whichever executor gets bound, never the daemon's own
	// disk, so its cwd is never stat-checked here; pin Kind explicitly so
	// this test keeps exercising the refusal it names.
	_, err := sp.Spawn(ctx, tools.SpawnSpec{Kind: protocol.KindClaude, Prompt: "x", Cwd: "/definitely/not/a/directory", Task: "1"})
	ck.Error(err, "want a refusal")

	rows, err := store.List(ctx, tasks.ListFilter{ConversationID: "conv-mine"})
	ck.NoError(err)
	ck.Len(rows, 1, "want 1 row, got %d", len(rows))
	ck.Eq("", rows[0].Assignee, "a refused spawn left task 1 assigned to")
}

func TestSpawnerSetBudgetAllowsADirectChild(t *testing.T) {
	ck := assert.NewAborting(t)
	c := spawnerFixture(t)
	sp := newControllerSpawner(c, "c_root") // c_mine is c_root's direct child
	ck.NoError(sp.SetBudget(context.Background(), "c_mine", 25.00), "SetBudget on a direct child must succeed")
	snap, _ := c.st.Get("c_mine")
	ck.Eq(25.00, snap.MaxCost, "c_mine.MaxCost")
}

// IMPORTANT: spawnerFixture's tree is c_root -> c_mine -> c_grandchild, plus
// an unrelated top-level c_stranger. Relative to c_mine, c_grandchild IS a
// direct child (see TestSpawnerListReturnsOnlyDescendants, which already
// asserts this) — so this test uses c_root as the caller, since
// c_grandchild is genuinely two hops away from c_root, not one.
func TestSpawnerSetBudgetRefusesAGrandchildAndAStrangerAndSelf(t *testing.T) {
	c := spawnerFixture(t)
	sp := newControllerSpawner(c, "c_root")
	for _, target := range []string{"c_grandchild", "c_stranger", "c_root", "c_nonexistent"} {
		assert.NewCollecting(t).Error(sp.SetBudget(context.Background(), target, 5.00), "SetBudget(%s) must refuse — none of these are c_root's DIRECT child", target)
	}
}

func TestSpawnerSetBudgetRequiresAnAgentID(t *testing.T) {
	c := spawnerFixture(t)
	sp := newControllerSpawner(c, "c_mine")
	assert.NewAborting(t).Error(sp.SetBudget(context.Background(), "", 5.00), "an empty agent id must be refused")
}
