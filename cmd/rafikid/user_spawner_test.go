package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// userSpawnerFixture builds a Controller with a hand-populated childstore, on
// the same principle as spawnerFixture: the authority checks must be
// observable against STORED state, with no live child.
//
//	a_top
//	 └── b_mid
//	      └── c_leaf
func userSpawnerFixture(t *testing.T) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	insert := func(id, parent string) {
		labels := map[string]string{}
		if parent != "" {
			labels[childstore.LabelParent] = parent
			labels[childstore.LabelRoot] = "a_top"
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Labels: labels,
			StartedAt: time.Now(), Kind: protocol.KindFundi,
		})
	}
	insert("a_top", "")
	insert("b_mid", "a_top")
	insert("c_leaf", "b_mid")
	return c
}

// A parented child's budget belongs to the agent that spawned it, at ANY
// depth: testing only the direct child would leave the grandchild case open,
// which is exactly the shape of the SetBudget hole the interface contract
// warns about.
func TestUserSpawnerSetBudgetRefusesEveryParentedChild(t *testing.T) {
	c := userSpawnerFixture(t)
	sp := newUserSpawner(c, users.Identity{UserID: "u_1", Username: "op"})
	ctx := context.Background()

	if err := sp.SetBudget(ctx, "a_top", 25.00); err != nil {
		t.Fatalf("SetBudget on the top-level agent must succeed: %v", err)
	}
	snap, _ := c.st.Get("a_top")
	if snap.MaxCost != 25.00 {
		t.Fatalf("a_top.MaxCost = %v, want 25.00", snap.MaxCost)
	}

	for _, tc := range []struct{ target, parent string }{
		{"b_mid", "a_top"},
		{"c_leaf", "b_mid"},
	} {
		err := sp.SetBudget(ctx, tc.target, 5.00)
		if err == nil {
			t.Errorf("SetBudget(%s) must refuse — it was spawned by %s", tc.target, tc.parent)
			continue
		}
		if !strings.Contains(err.Error(), tc.parent) {
			t.Errorf("SetBudget(%s) refusal must name the parent %s; got %v", tc.target, tc.parent, err)
		}
	}
}

func TestUserSpawnerListReportsAbsoluteDepth(t *testing.T) {
	sp := newUserSpawner(userSpawnerFixture(t), users.Identity{UserID: "u_1", Username: "op"})
	kids, err := sp.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 {
		t.Fatalf("want all three children, got %+v", kids)
	}
	for i, want := range []struct {
		id    string
		depth int
	}{
		{"a_top", 0}, {"b_mid", 1}, {"c_leaf", 2},
	} {
		if kids[i].ChildID != want.id {
			t.Errorf("kids[%d].ChildID = %s, want %s", i, kids[i].ChildID, want.id)
		}
		if kids[i].Depth != want.depth {
			t.Errorf("kids[%d] (%s).Depth = %d, want %d", i, kids[i].ChildID, kids[i].Depth, want.depth)
		}
	}
}

// A refusal for cwd must happen before the controller is asked, so no spawn
// is even attempted: the store must still hold only the fixture's children.
func TestUserSpawnerSpawnRefusesRelativeAndEmptyCwd(t *testing.T) {
	c := userSpawnerFixture(t)
	sp := newUserSpawner(c, users.Identity{UserID: "u_1", Username: "op"})
	ctx := context.Background()

	for _, cwd := range []string{"", "relative/path"} {
		_, err := sp.Spawn(ctx, tools.SpawnSpec{Cwd: cwd, Prompt: "x"})
		if err == nil {
			t.Errorf("Spawn with cwd %q must refuse", cwd)
		} else if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("Spawn with cwd %q must refuse for absoluteness; got %v", cwd, err)
		}
	}
	if got := len(c.st.List()); got != 3 {
		t.Fatalf("a refused spawn must not be attempted; store holds %d children, want 3", got)
	}
}

func TestUserSpawnerSpawnRefusesTaskAtSpawn(t *testing.T) {
	c := userSpawnerFixture(t)
	sp := newUserSpawner(c, users.Identity{UserID: "u_1", Username: "op"})

	// A valid absolute cwd, so the ONLY refusal this spec can trigger is the
	// task one — the assertion would otherwise pass for the wrong reason.
	_, err := sp.Spawn(context.Background(), tools.SpawnSpec{Cwd: "/tmp", Task: "T-1"})
	if err == nil {
		t.Fatal("task assignment at spawn must refuse")
	}
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("refusal must name the reason; got %v", err)
	}
	if got := len(c.st.List()); got != 3 {
		t.Fatalf("the refusal must fire before the controller is reached; store holds %d children, want 3", got)
	}
}

func TestUserSpawnerViewSendKillRejectUnknownChild(t *testing.T) {
	sp := newUserSpawner(userSpawnerFixture(t), users.Identity{UserID: "u_1", Username: "op"})
	ctx := context.Background()

	if _, err := sp.View(ctx, "c_missing", 0); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("View(c_missing) must reject an unregistered id; got %v", err)
	}
	if err := sp.Send(ctx, "c_missing", "hi"); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("Send(c_missing) must reject an unregistered id; got %v", err)
	}
	if err := sp.Kill(ctx, "c_missing"); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("Kill(c_missing) must reject an unregistered id; got %v", err)
	}
}
