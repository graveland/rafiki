package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func intp(v int) *int { return &v }

// limitsFixture builds a controller whose childstore holds a chain of the
// requested depth, each link granting the depth given.
//
//	c_d0 (depth 0 in the tree) -> c_d1 -> c_d2 -> ...
func limitsFixture(t *testing.T, grants ...int) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	prev, root := "", ""
	for i, g := range grants {
		id := "c_d" + string(rune('0'+i))
		labels := map[string]string{}
		if prev != "" {
			labels[childstore.LabelParent] = prev
			labels[childstore.LabelRoot] = root
		} else {
			root = id
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Labels: labels,
			StartedAt: time.Now(), Kind: protocol.KindFundi,
			MaxDepth: g, MaxChildren: 4,
		})
		prev = id
	}
	return c
}

func TestAbsoluteDepthCountsStoredParentLinks(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 2, 1)
	for id, want := range map[string]int{"c_d0": 0, "c_d1": 1, "c_d2": 2} {
		got := c.st.AbsoluteDepth(id)
		ck.Eq(want, got, "AbsoluteDepth(%s) = %d, want", id, got)
	}
	ck.Eq(-1, c.st.AbsoluteDepth("c_unknown"), "an unknown child must report -1, got")
}

// deepChainFixture builds a controller whose childstore holds a parent chain
// one hop deeper than childstore's walk bound (maxChainDepth is 64; the
// literal is mirrored here because the constant is unexported):
//
//	c_x0 (top) -> c_x1 -> ... -> c_x65
//
// AbsoluteDepth(c_x65) therefore cannot resolve and must report the refuse
// sentinel, never the bound itself.
func deepChainFixture(t *testing.T) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	prev := ""
	for i := 0; i < 66; i++ {
		id := fmt.Sprintf("c_x%d", i)
		labels := map[string]string{}
		if i > 0 {
			labels[childstore.LabelParent] = prev
			labels[childstore.LabelRoot] = "c_x0"
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Labels: labels,
			StartedAt: time.Now(), Kind: protocol.KindFundi,
			MaxDepth: 3, MaxChildren: 4,
		})
		prev = id
	}
	return c
}

// A parent chain deeper than the walk bound must fail closed, not report the
// bound as a real depth: before AbsoluteDepth grew its sentinel, a 65+-hop
// chain surfaced as a perfectly actionable depth 64 and sailed through every
// depth check below the ceiling.
func TestTruncatedParentChainRefusesSpawn(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := deepChainFixture(t)

	ck.Require().Eq(-1, c.st.AbsoluteDepth("c_x65"), "AbsoluteDepth(c_x65)")
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_x65"})
	ck.Require().Error(err, "a parent whose depth cannot be determined must be refused, not treated as top level")
	ck.NotStrContains(err.Error(), "no such child", "the child exists; the refusal must name the undeterminable depth: %v", err)
	ck.StrContains(err.Error(), "depth", "refusal must name the limit: %v", err)
}

// The grant arithmetic must fail closed on an indeterminate landing depth
// too: childDepthFor reports -1 rather than assuming top level, and
// grantedDepth turns that into a grant of 0 — the same "0 means cannot
// spawn" convention grantedChildren uses. Without the clamp the room
// arithmetic reads the -1 as bonus room and mints the requested grant.
func TestIndeterminateLandingDepthGrantsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := deepChainFixture(t)

	depth := childDepthFor(c.st, "c_x65")
	ck.Require().Eq(-1, depth, "childDepthFor(c_x65)")
	ask := 2
	ck.Eq(0, grantedDepth(protocol.SpawnRequest{MaxDepth: &ask}, depth, resolveAbsoluteDepthCeiling()), "a child of an indeterminate-depth parent must be granted depth 0 (cannot spawn), got")
}

// depth 0 means "cannot spawn". This is the base case and the one an
// off-by-one gets wrong in the permissive direction.
func TestDepthZeroCannotSpawn(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 0)
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"})
	ck.Require().Error(err, "an agent granted depth 0 must not be able to spawn")
	ck.StrContains(err.Error(), "depth", "refusal must name the limit: %v", err)
}

func TestDepthOneCanSpawnOnce(t *testing.T) {
	c := limitsFixture(t, 1)
	assert.NewAborting(t).NoError(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "depth 1 must permit one hop")
}

// THE case the "grant locally, bound absolutely" split exists for, taken
// verbatim from the design's testing section: a grant of 2 from a child
// already at absolute depth 2 is refused by the ceiling even though the
// parent's own grant permits it.
func TestAbsoluteCeilingBeatsAParentsGrant(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "3")
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 5, 5, 5) // c_d2 sits at absolute depth 2 and grants freely

	// Child would land at absolute depth 3, which is the ceiling itself —
	// allowed, since the ceiling is the deepest permitted position.
	ck.Require().NoError(c.checkSpawnLimits(protocol.SpawnRequest{
		ParentChildID: "c_d2", MaxDepth: intp(2),
	}), "landing exactly at the ceiling must be allowed")

	// One deeper: the grandchild would land at absolute depth 4.
	c.st.Insert(&childstore.Session{
		ChildID: "c_d3", Status: protocol.StatusIdle, StartedAt: time.Now(),
		MaxDepth: 5,
		Labels: map[string]string{
			childstore.LabelParent: "c_d2", childstore.LabelRoot: "c_d0",
		},
	})
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d3", MaxDepth: intp(2)})
	ck.Require().Error(err, "RAFIKI_MAX_DEPTH must refuse regardless of what the parent granted")
	ck.StrContains(err.Error(), "RAFIKI_MAX_DEPTH", "refusal must name the ceiling so it is diagnosable: %v", err)
}

// Forge the request. This is the assertion that separates a real boundary from
// UX: construct a SpawnRequest with a depth the parent never had and drive it
// straight at the controller, bypassing every tool.
func TestForgedDepthGrantIsRefused(t *testing.T) {
	c := limitsFixture(t, 0) // parent granted zero
	err := c.checkSpawnLimits(protocol.SpawnRequest{
		ParentChildID: "c_d0",
		MaxDepth:      intp(99), // "please give my child 99 levels"
	})
	assert.NewAborting(t).Error(err, "a check that only ran in the tool would pass a test driven through the tool; this one is not")
}

// A top-level spawn (no parent) is not depth-limited by a parent that does not
// exist, but is still bounded by the absolute ceiling.
func TestTopLevelSpawnIsUnparented(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "3")
	c := limitsFixture(t)
	assert.NewAborting(t).NoError(c.checkSpawnLimits(protocol.SpawnRequest{}), "a top-level spawn must be permitted")
}

func TestMaxDepthCeilingDefaultsToThree(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "")
	c := assert.NewAborting(t)
	c.Eq(3, resolveAbsoluteDepthCeiling(), "default ceiling")
	t.Setenv("RAFIKI_MAX_DEPTH", "not-a-number")
	c.Eq(3, resolveAbsoluteDepthCeiling(), "an unparseable ceiling must fall back to 3, got")
	t.Setenv("RAFIKI_MAX_DEPTH", "0")
	c.Eq(0, resolveAbsoluteDepthCeiling(), "an explicit 0 must disable spawning entirely, got")
}

func insertChild(c *Controller, id, parent, root string, status protocol.Status) {
	c.st.Insert(&childstore.Session{
		ChildID: id, Status: status, StartedAt: time.Now(), MaxDepth: 2, MaxChildren: 4,
		Labels: map[string]string{
			childstore.LabelParent: parent, childstore.LabelRoot: root,
		},
	})
}

func TestConcurrencyCapRefusesTheFifthChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3) // c_d0, MaxChildren 4
	for _, id := range []string{"c_w1", "c_w2", "c_w3", "c_w4"} {
		insertChild(c, id, "c_d0", "c_d0", protocol.StatusIdle)
	}
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"})
	ck.Require().Error(err, "the fifth live child must be refused")
	ck.StrContains(err.Error(), "4", "refusal must name the cap so it is actionable: %v", err)
}

// The cap counts LIVE descendants. An exited child frees a slot — otherwise a
// long-running coordinator that has cycled through twenty workers can never
// spawn again, which is a leak dressed as a limit.
func TestExitedChildFreesASlot(t *testing.T) {
	c := limitsFixture(t, 3)
	for _, id := range []string{"c_w1", "c_w2", "c_w3"} {
		insertChild(c, id, "c_d0", "c_d0", protocol.StatusIdle)
	}
	insertChild(c, "c_dead", "c_d0", "c_d0", protocol.StatusExited)

	assert.NewAborting(t).NoError(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "3 live + 1 exited is under a cap of 4")
}

// "Across the subtree", not "direct children". A coordinator with two workers
// that each hold two reviewers is at four, not two.
func TestConcurrencyCountsTheWholeSubtree(t *testing.T) {
	c := limitsFixture(t, 3)
	insertChild(c, "c_w1", "c_d0", "c_d0", protocol.StatusIdle)
	insertChild(c, "c_w2", "c_d0", "c_d0", protocol.StatusIdle)
	insertChild(c, "c_r1", "c_w1", "c_d0", protocol.StatusIdle)
	insertChild(c, "c_r2", "c_w2", "c_d0", protocol.StatusIdle)

	assert.NewAborting(t).Error(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "4 live descendants at any depth must exhaust a cap of 4")
}

func TestZeroMaxChildrenBlocksEverySpawn(t *testing.T) {
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxChildren = 0 })
	assert.NewAborting(t).Error(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "max-children 0 must refuse")
}

// fakeCoster stands in for insights so the admission logic is testable
// without a database.
type fakeCoster struct {
	spend float64
	err   error
}

func (f fakeCoster) SubtreeCost(context.Context, insights.SubtreeSelector) (float64, error) {
	return f.spend, f.err
}

func (f fakeCoster) CostsByConversation(context.Context, insights.SubtreeSelector) ([]insights.ConversationCost, error) {
	return nil, nil
}

func TestBudgetExhaustedRefusesSpawn(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })
	c.coster = fakeCoster{spend: 5.01}

	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"})
	ck.Require().Error(err, "a subtree over budget must not spawn more agents")
	for _, want := range []string{"5.01", "5.00", "budget"} {
		ck.StrContains(err.Error(), want, "refusal must show spend, budget and the limit name; missing %q in %v", want, err)
	}
}

func TestChildCannotBeGrantedMoreThanTheParentHasLeft(t *testing.T) {
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.coster = fakeCoster{spend: 9.00}

	cost := 5.00
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0", MaxCost: &cost})
	assert.NewAborting(t).Error(err, "a $5 grant from a parent with $1 remaining must be refused")
}

func TestUnbudgetedParentIsUnlimited(t *testing.T) {
	c := limitsFixture(t, 3)
	c.coster = fakeCoster{spend: 1000.00}
	assert.NewAborting(t).NoError(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "an unbudgeted parent must not be limited")
}

func TestCostLookupFailureRefusesABudgetedSpawn(t *testing.T) {
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })
	c.coster = fakeCoster{err: errors.New("db down")}

	assert.NewAborting(t).Error(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "an unreadable budget must fail closed for a BUDGETED parent")
}

func TestCostLookupFailureDoesNotBlockAnUnbudgetedSpawn(t *testing.T) {
	c := limitsFixture(t, 3)
	c.coster = fakeCoster{err: errors.New("db down")}
	assert.NewAborting(t).NoError(c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}), "an unbudgeted parent must not be blocked by a cost query")
}

// 0 means UNLIMITED in storage, so collapsing a negative to 0 mints an
// unbudgeted child — and every descendant with it, since checkBudget returns
// nil the moment the parent is unbudgeted. grantedDepth and grantedChildren
// collapse negatives to 0 and thereby fail CLOSED; only cost inverts, because
// only cost overloads 0.
func TestNegativeMaxCostIsRefusedNotTreatedAsUnlimited(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.coster = fakeCoster{spend: 9.97}

	neg := -1.0
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0", MaxCost: &neg})
	ck.Require().Error(err, "a negative budget must be refused, not collapsed to unlimited")
	ck.StrContains(err.Error(), "negative", "the refusal must name the problem: %v", err)
}

// A malformed argument is malformed whatever the parent's own budget is. Both
// of these reach checkBudget's early returns, so a check placed after them
// would leave the escape open on exactly the paths where nothing else bounds
// the child.
func TestNegativeMaxCostIsRefusedUnderAnUnbudgetedParent(t *testing.T) {
	c := limitsFixture(t, 3) // c_d0 has MaxCost 0 = unlimited
	neg := -1.0
	assert.NewAborting(t).Error(c.checkSpawnLimits(protocol.SpawnRequest{
		ParentChildID: "c_d0", MaxCost: &neg,
	}), "an unbudgeted parent must still not be able to mint a negative budget")
}

func TestNegativeMaxCostIsRefusedForATopLevelSpawn(t *testing.T) {
	c := limitsFixture(t)
	neg := -0.01
	assert.NewAborting(t).Error(c.checkSpawnLimits(protocol.SpawnRequest{MaxCost: &neg}), "a top-level spawn must not be able to mint a negative budget either")
}

// The mirror image, asserted rather than assumed: a negative depth or child
// cap must mean "cannot spawn", never "unlimited". These pass today; they are
// here so a future symmetry-minded cleanup of grantedCost cannot quietly
// invert them to match.
func TestNegativeDepthAndChildrenGrantsFailClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	neg := -1
	c.Eq(0, grantedDepth(protocol.SpawnRequest{MaxDepth: &neg}, 0, 3), "grantedDepth(-1)")
	c.Eq(0, grantedChildren(protocol.SpawnRequest{MaxChildren: &neg}), "grantedChildren(-1)")
}
