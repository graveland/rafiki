// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// budgetHalted is the ancestor walk over c.breaches. A worker under a healthy
// coordinator, with no breach anywhere, is never halted.
func TestBudgetHaltedFalseWithNoBreach(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	root, halted := c.budgetHalted("c_w1")
	ck.False(halted, "budgetHalted with no breach anywhere must not halt")
	ck.Eq("", root, "root")
}

// The child's OWN breach halts it, naming the child as the root.
func TestBudgetHaltedOnOwnBreach(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	c.breaches.mark("c_w1")
	root, halted := c.budgetHalted("c_w1")
	ck.True(halted, "own breach must halt")
	ck.Eq("c_w1", root, "root")
}

// A breach recorded only on the ANCESTOR halts the descendant too: the walk is
// the predicate, and removing it makes this fail.
func TestBudgetHaltedWalksAncestors(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	c.breaches.mark("c_coord")
	root, halted := c.budgetHalted("c_w1")
	ck.True(halted, "ancestor breach must halt the descendant")
	ck.Eq("c_coord", root, "root must be the breached node, not the asked child")

	// The walker itself, by contrast, is NOT halted: no breach above it.
	root, halted = c.budgetHalted("c_coord")
	ck.True(halted, "the breached node halts itself")
	ck.Eq("c_coord", root, "root")
}

// A corrupt parent chain that names itself in a cycle must not wedge the walk:
// the seen map terminates it, and the call returns.
func TestBudgetHaltedSurvivesAParentCycle(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_cycA", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "c_cycB"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_cycB", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "c_cycA"},
	})
	// Unmarked cycle: the walk ends through the seen guard.
	root, halted := c.budgetHalted("c_cycA")
	ck.False(halted, "a cycle with no breach must return, unhalted")
	ck.Eq("", root, "root")
	// Breach inside the cycle: the walk still terminates, at the breach.
	c.breaches.mark("c_cycB")
	root, halted = c.budgetHalted("c_cycA")
	ck.True(halted, "a breach inside the cycle must be found")
	ck.Eq("c_cycB", root, "root")
}

// seedBudgetBreaches marks a breached root WITHOUT steering: the steer was
// delivered by the previous incarnation and rides the durable inbox, so the
// seed must not re-send it — no fragment containing "budget" is ever flushed.
func TestSeedBudgetBreachesMarksWithoutSteering(t *testing.T) {
	c, clk, cap := settleFixture(t)
	ck := assert.NewAborting(t)
	c.coster = fakeCoster{spend: 99}
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10 })

	c.seedBudgetBreaches(context.Background())
	ck.True(c.breaches.has("c_coord"), "an at-cap subtree must be marked breached")

	clk.Advance(10 * time.Second) // past the 5s debounce
	for _, b := range cap.batches() {
		for _, f := range b.fragments {
			ck.NotStrContains(f, "budget", "the seed must not steer; a fragment leaked: %q", f)
		}
	}
	ck.Len(cap.batches(), 0, "the seed must not push any event at all")
}

// sweepBudgets skips exited rows; the seed may not — a subtree whose root's
// row recovered as exited is still over budget, and its resumed (or later
// manually resumed) descendants must be gated.
func TestSeedBudgetBreachesIncludesExitedRoots(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	c.coster = fakeCoster{spend: 99}
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10 })
	c.st.SetStatus("c_coord", protocol.StatusExited)

	c.seedBudgetBreaches(context.Background())
	ck.True(c.breaches.has("c_coord"), "an exited root over budget must still be marked")
}

// A node with no budget (MaxCost 0 = unlimited) is never marked, however much
// its subtree spent.
func TestSeedBudgetBreachesSkipsUnbudgeted(t *testing.T) {
	c, _, _ := settleFixture(t)
	ck := assert.NewAborting(t)
	c.coster = fakeCoster{spend: 99}
	// settleFixture leaves c_coord's MaxCost at 0.

	c.seedBudgetBreaches(context.Background())
	ck.False(c.breaches.has("c_coord"), "an unbudgeted node must never be marked")
}

// seedChildStore fronts the boot walk with rows to recover: a
// childstore.ChildStore whose List returns a fixed set.
type seedChildStore struct {
	stubChildStore
	recs []childstore.ChildRecord
}

func (s *seedChildStore) List(context.Context) ([]childstore.ChildRecord, error) {
	return s.recs, nil
}

// The seed rides loadChildren's defer, not a separate boot step — so the
// binding must be pinned where deleting the defer fails: a full loadChildren
// walk over a record that the database says is over budget must leave the
// breach marked when the function returns.
func TestSeedBudgetBreachesOnLoadChildren(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	c.coster = fakeCoster{spend: 99}
	c.children = &seedChildStore{recs: []childstore.ChildRecord{{
		ChildID: "c_coord", Kind: protocol.KindFundi,
		Status:  string(protocol.StatusExited), // planStayExited: no resume machinery in a unit test
		MaxCost: 10, SpawnedAt: time.Now(),
	}}}

	c.loadChildren(context.Background())
	ck.True(c.breaches.has("c_coord"),
		"loadChildren's seed must mark an over-budget recovered root; deleting the defer fails here")

	// Marking only: no steer was (re-)sent for a breach the previous
	// incarnation already delivered.
	clk.Advance(10 * time.Second) // past the 5s debounce
	ck.Len(cap.batches(), 0, "the boot seed must not push any event")
}
