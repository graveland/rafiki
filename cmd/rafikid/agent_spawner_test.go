package main

import (
	"context"
	"fmt"
	"strings"
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
	kids, err := sp.List(context.Background(), tools.ListOpts{})
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
		if _, err := sp.View(ctx, tools.ViewSpec{ChildID: target}); err == nil {
			t.Errorf("View(%s) must refuse", target)
		}
		_, sErr := sp.Send(ctx, tools.SendSpec{ChildID: target, Message: "hi"})
		c.Error(sErr, "Send(%s) must refuse", target)
		c.Error(sp.Kill(ctx, target), "Kill(%s) must refuse", target)
	}
}

func TestSpawnerRefusalNamesTheTarget(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	_, err := sp.Send(context.Background(), tools.SendSpec{ChildID: "c_stranger", Message: "hi"})
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

// --- owner-scoped READS (ownerReadBase's fixture: c_a/c_a1 and c_b/c_b1 are
// owner u1's two top-level trees, c_c is u2's, c_d/c_d1 and c_f are unowned) ---

// A same-owner sibling in another top-level tree is READABLE. This is the whole
// widening: authorize (which every act verb still uses) refuses exactly this
// target, and the descendant case keeps working beside it.
func TestControllerSpawnerReadViewAdmitsSameOwnerSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(ownerReadBase(t), "c_a")
	ctx := context.Background()

	out, err := sp.View(ctx, tools.ViewSpec{ChildID: "c_b"})
	c.Require().NoError(err, "a same-owner sibling must be readable")
	c.NotStrContains(out, "not one you can read", "the read must be admitted, not refused; got:\n%s", out)

	_, err = sp.View(ctx, tools.ViewSpec{ChildID: "c_a1"})
	c.Require().NoError(err, "a descendant is still readable")
}

// A foreign owner's agent, an unknown id and an empty id all refuse — and the
// first two are INDISTINGUISHABLE, so a caller cannot learn which it hit.
func TestControllerSpawnerReadViewRefusesForeignAndUnknownIdentically(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(ownerReadBase(t), "c_a")
	ctx := context.Background()

	_, foreign := sp.View(ctx, tools.ViewSpec{ChildID: "c_c"})
	_, unknown := sp.View(ctx, tools.ViewSpec{ChildID: "c_nope"})
	c.Require().Error(foreign, "a foreign owner's agent must not be readable")
	c.Require().Error(unknown, "an unknown id must not be readable")
	// The sentence is templated with the target's id, so "identical" means
	// identical but for the id it names: nothing in it tells the caller whether
	// the target exists and belongs to somebody else, or does not exist.
	withoutID := func(err error, target string) string {
		return strings.Replace(err.Error(), target, "<target>", 1)
	}
	c.Eq(withoutID(foreign, "c_c"), withoutID(unknown, "c_nope"),
		"a foreign target and an unknown one must get the same refusal, got")
	c.StrContains(foreign.Error(), fmt.Sprintf(notReadableText, "c_c"),
		"the refusal is the shared read sentence naming the target; got %v", foreign)

	// The empty id is a different, older refusal: it is a caller mistake, not a
	// permission answer.
	_, empty := sp.View(ctx, tools.ViewSpec{})
	c.Require().Error(empty, "an empty target must be refused")
	c.StrContains(empty.Error(), "agent id is required", "the empty-id refusal names the problem; got %v", empty)
}

// A target with no owner anywhere in its lineage is admin/UDS-only, exactly as
// for recall: an ownerful caller may not read it, nor anything beneath it.
func TestControllerSpawnerReadViewRefusesUnownedLineage(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(ownerReadBase(t), "c_a")
	ctx := context.Background()

	for _, target := range []string{"c_d", "c_d1", "c_f"} {
		_, err := sp.View(ctx, tools.ViewSpec{ChildID: target})
		c.Require().Error(err, "%s has no owner in its lineage and must not be readable", target)
		c.StrContains(err.Error(), fmt.Sprintf(notReadableText, target),
			"the refusal is the shared read sentence; got %v", err)
	}
}

// An ownerless caller has no owner scope at all: an owner-scope list falls back
// to its subtree alone, and those rows are still marked as its own.
func TestControllerSpawnerReadListOwnerScopeWithoutOwnerFallsBackToSubtree(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(ownerReadBase(t), "c_d")

	kids, err := sp.List(context.Background(), tools.ListOpts{Scope: tools.ListScopeOwner})
	c.Require().NoError(err)
	c.Require().Len(kids, 1, "want only the caller's own descendant, got %+v", kids)
	c.Eq("c_d1", kids[0].ChildID, "kids[0].ChildID")
	c.Require().NotNil(kids[0].InSubtree, "an owner-scope row is marked, got")
	c.True(*kids[0].InSubtree, "the fallback row is in the caller's own subtree, got")
}

// An owner-scope list puts the caller's subtree first in the usual order, then
// its owner's other agents at depth 0 marked read-only. A foreign owner's agent
// is in neither, and the plain scope is unchanged.
func TestControllerSpawnerReadListOwnerScopeOrdersSubtreeFirstAndMarksReadOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ctrl := ownerReadFixture(t,
		ownerReadChild{id: "c_a", owner: "u1", started: t0},
		ownerReadChild{id: "c_a1", parent: "c_a", started: t0.Add(1 * time.Minute)},
		ownerReadChild{id: "c_a2", parent: "c_a", started: t0.Add(2 * time.Minute)},
		ownerReadChild{id: "c_b", owner: "u1", started: t0.Add(3 * time.Minute)},
		ownerReadChild{id: "c_c", owner: "u2", started: t0.Add(4 * time.Minute)},
	)
	sp := newControllerSpawner(ctrl, "c_a")

	kids, err := sp.List(context.Background(), tools.ListOpts{Scope: tools.ListScopeOwner})
	c.Require().NoError(err)

	ids := make([]string, 0, len(kids))
	marked := make([]bool, 0, len(kids))
	depths := make([]int, 0, len(kids))
	for _, k := range kids {
		ids = append(ids, k.ChildID)
		marked = append(marked, k.InSubtree != nil && *k.InSubtree)
		depths = append(depths, k.Depth)
	}
	// Within the subtree, sortAgents puts a shallower row first and breaks a
	// depth tie newest-first; the owner's other agent follows at depth 0.
	c.EqDeep([]string{"c_a2", "c_a1", "c_b"}, ids, "owner scope orders the subtree first, then the owner's others")
	c.EqDeep([]bool{true, true, false}, marked, "subtree rows are marked true and the others false")
	c.EqDeep([]int{1, 1, 0}, depths, "an agent outside the subtree is not indented as if it were beneath the caller")
	c.NotContains(ids, "c_c", "a foreign owner's agent must not be listed")

	plain, err := sp.List(context.Background(), tools.ListOpts{Scope: tools.ListScopeSubtree})
	c.Require().NoError(err)
	c.Require().Len(plain, 2, "the subtree scope is unchanged, got %+v", plain)
	c.Eq("c_a2", plain[0].ChildID, "plain[0].ChildID")
	for _, k := range plain {
		c.Nil(k.InSubtree, "a subtree list makes no InSubtree claim; %s has %v", k.ChildID, k.InSubtree)
	}
}

// The act verbs still require the spawn tree: the same-owner sibling View now
// admits is refused by Send and Kill, so the widening never becomes authority
// to steer another agent's worker.
func TestControllerSpawnerReadSendAndKillStillRefuseSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := newControllerSpawner(ownerReadBase(t), "c_a")
	ctx := context.Background()

	_, err := sp.Send(ctx, tools.SendSpec{ChildID: "c_b", Message: "hi"})
	c.Require().Error(err, "agent_send must refuse a same-owner sibling")
	c.StrContains(err.Error(), "descendant", "the act refusal is still the spawn-tree one; got %v", err)
	c.Error(sp.Kill(ctx, "c_b"), "agent_kill must refuse a same-owner sibling")

	_, err = sp.View(ctx, tools.ViewSpec{ChildID: "c_b"})
	c.Require().NoError(err, "the same target must remain readable")
}
