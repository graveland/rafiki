package main

import (
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	c := limitsFixture(t, 3, 2, 1)
	for id, want := range map[string]int{"c_d0": 0, "c_d1": 1, "c_d2": 2} {
		if got := c.st.AbsoluteDepth(id); got != want {
			t.Errorf("AbsoluteDepth(%s) = %d, want %d", id, got, want)
		}
	}
	if got := c.st.AbsoluteDepth("c_unknown"); got != -1 {
		t.Errorf("an unknown child must report -1, got %d", got)
	}
}

// depth 0 means "cannot spawn". This is the base case and the one an
// off-by-one gets wrong in the permissive direction.
func TestDepthZeroCannotSpawn(t *testing.T) {
	c := limitsFixture(t, 0)
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"})
	if err == nil {
		t.Fatal("an agent granted depth 0 must not be able to spawn")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("refusal must name the limit: %v", err)
	}
}

func TestDepthOneCanSpawnOnce(t *testing.T) {
	c := limitsFixture(t, 1)
	if err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"}); err != nil {
		t.Fatalf("depth 1 must permit one hop: %v", err)
	}
}

// THE case the "grant locally, bound absolutely" split exists for, taken
// verbatim from the design's testing section: a grant of 2 from a child
// already at absolute depth 2 is refused by the ceiling even though the
// parent's own grant permits it.
func TestAbsoluteCeilingBeatsAParentsGrant(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "3")
	c := limitsFixture(t, 5, 5, 5) // c_d2 sits at absolute depth 2 and grants freely

	// Child would land at absolute depth 3, which is the ceiling itself —
	// allowed, since the ceiling is the deepest permitted position.
	if err := c.checkSpawnLimits(protocol.SpawnRequest{
		ParentChildID: "c_d2", MaxDepth: intp(2),
	}); err != nil {
		t.Fatalf("landing exactly at the ceiling must be allowed: %v", err)
	}

	// One deeper: the grandchild would land at absolute depth 4.
	c.st.Insert(&childstore.Session{
		ChildID: "c_d3", Status: protocol.StatusIdle, StartedAt: time.Now(),
		MaxDepth: 5,
		Labels: map[string]string{
			childstore.LabelParent: "c_d2", childstore.LabelRoot: "c_d0",
		},
	})
	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d3", MaxDepth: intp(2)})
	if err == nil {
		t.Fatal("RAFIKI_MAX_DEPTH must refuse regardless of what the parent granted")
	}
	if !strings.Contains(err.Error(), "RAFIKI_MAX_DEPTH") {
		t.Errorf("refusal must name the ceiling so it is diagnosable: %v", err)
	}
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
	if err == nil {
		t.Fatal("a check that only ran in the tool would pass a test driven through the tool; this one is not")
	}
}

// A top-level spawn (no parent) is not depth-limited by a parent that does not
// exist, but is still bounded by the absolute ceiling.
func TestTopLevelSpawnIsUnparented(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "3")
	c := limitsFixture(t)
	if err := c.checkSpawnLimits(protocol.SpawnRequest{}); err != nil {
		t.Fatalf("a top-level spawn must be permitted: %v", err)
	}
}

func TestMaxDepthCeilingDefaultsToThree(t *testing.T) {
	t.Setenv("RAFIKI_MAX_DEPTH", "")
	if got := resolveAbsoluteDepthCeiling(); got != 3 {
		t.Fatalf("default ceiling = %d, want 3", got)
	}
	t.Setenv("RAFIKI_MAX_DEPTH", "not-a-number")
	if got := resolveAbsoluteDepthCeiling(); got != 3 {
		t.Fatalf("an unparseable ceiling must fall back to 3, got %d", got)
	}
	t.Setenv("RAFIKI_MAX_DEPTH", "0")
	if got := resolveAbsoluteDepthCeiling(); got != 0 {
		t.Fatalf("an explicit 0 must disable spawning entirely, got %d", got)
	}
}
