package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	kids, err := sp.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].ChildID != "c_grandchild" {
		t.Fatalf("want only c_grandchild, got %+v", kids)
	}
	if kids[0].Depth != 1 {
		t.Errorf("want depth 1, got %d", kids[0].Depth)
	}
}

func TestSpawnerRefusesNonDescendantOnEveryVerb(t *testing.T) {
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	ctx := context.Background()

	// A sibling tree, the caller's own parent, and the caller itself are all
	// non-descendants. A check that only rejected the first would pass a test
	// that only tried the first.
	for _, target := range []string{"c_stranger", "c_root", "c_mine", "c_nonexistent"} {
		if _, err := sp.View(ctx, target, 0); err == nil {
			t.Errorf("View(%s) must refuse", target)
		}
		if err := sp.Send(ctx, target, "hi"); err == nil {
			t.Errorf("Send(%s) must refuse", target)
		}
		if err := sp.Kill(ctx, target); err == nil {
			t.Errorf("Kill(%s) must refuse", target)
		}
	}
}

func TestSpawnerRefusalNamesTheTarget(t *testing.T) {
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	err := sp.Send(context.Background(), "c_stranger", "hi")
	if err == nil {
		t.Fatal("want a refusal")
	}
	// An error message is a prompt that is only paid when it is needed
	// (prompting.md). It must say which id was rejected and why.
	if !strings.Contains(err.Error(), "c_stranger") {
		t.Errorf("refusal must name the target; got %v", err)
	}
	if !strings.Contains(err.Error(), "descendant") {
		t.Errorf("refusal must say why; got %v", err)
	}
}
