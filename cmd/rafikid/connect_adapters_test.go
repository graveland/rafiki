// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
)

// capturingCoster records the selector it was handed.
type capturingCoster struct {
	sel  insights.SubtreeSelector
	rows []insights.ConversationCost
}

func (c *capturingCoster) SubtreeCost(context.Context, insights.SubtreeSelector) (float64, error) {
	return 0, nil
}

func (c *capturingCoster) CostsByConversation(
	_ context.Context, sel insights.SubtreeSelector,
) ([]insights.ConversationCost, error) {
	c.sel = sel
	return c.rows, nil
}

// The two correlation routes are not interchangeable. A conversation is found
// by UUID for a fundi child (SessionID) and by external_ref for a proxy child,
// where the daemon sets X-Rafiki-Session to the CHILD id. Handing SessionID to
// both routes makes every proxy child match neither and roll up a non-nil
// zero, which then overwrites cost the rail accumulated from turn_end.
func TestCostsForCorrelatesExternalRefByChildID(t *testing.T) {
	cap := &capturingCoster{}
	c := &Controller{coster: cap}

	c.costsFor([]childstore.Snapshot{
		{ChildID: "c_fundi", SessionID: "11111111-1111-1111-1111-111111111111"},
		{ChildID: "c_proxy"},
	})

	if got, want := cap.sel.ConversationIDs, []string{"11111111-1111-1111-1111-111111111111"}; !slices.Equal(got, want) {
		t.Errorf("ConversationIDs = %v, want %v (the SESSION id)", got, want)
	}
	if got, want := cap.sel.ExternalRefs, []string{"c_fundi", "c_proxy"}; !slices.Equal(got, want) {
		t.Errorf("ExternalRefs = %v, want %v (the CHILD id, for every child)", got, want)
	}
}

// One round trip for the whole list, not one per child.
func TestCostsForIssuesASingleRollup(t *testing.T) {
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "22222222-2222-2222-2222-222222222222", Cost: 2.0},
		{ConversationID: "33333333-3333-3333-3333-333333333333", ExternalRef: "c_proxy", Cost: 5.0},
	}}
	c := &Controller{coster: cap}

	got := c.costsFor([]childstore.Snapshot{
		{ChildID: "c_fundi", SessionID: "22222222-2222-2222-2222-222222222222"},
		{ChildID: "c_proxy"},
		{ChildID: "c_idle", SessionID: "44444444-4444-4444-4444-444444444444"},
	})

	if got["c_fundi"] != 2.0 {
		t.Errorf("c_fundi = %v, want 2.0 (matched by conversation UUID)", got["c_fundi"])
	}
	if got["c_proxy"] != 5.0 {
		t.Errorf("c_proxy = %v, want 5.0 (matched by external_ref)", got["c_proxy"])
	}
	// Present and zero is a real answer -- the query ran and found no turns --
	// and is distinct from absent, which leaves CostUSD nil.
	if v, ok := got["c_idle"]; !ok || v != 0 {
		t.Errorf("c_idle = (%v, %v), want (0, true)", v, ok)
	}
}

// A child reachable by BOTH routes resolves to one conversation row and must
// be counted once.
func TestCostsForCountsOneConversationOnce(t *testing.T) {
	conv := "55555555-5555-5555-5555-555555555555"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: conv, ExternalRef: "c_both", Cost: 4.0},
	}}
	c := &Controller{coster: cap}

	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_both", SessionID: conv}})
	if got["c_both"] != 4.0 {
		t.Errorf("c_both = %v, want 4.0: matching both routes must not double it", got["c_both"])
	}
}

// A thread branch with no child of its own is a tool call the parent made --
// Claude Code's WebFetch summarizer and WebSearch driver fork a branch each and
// get no synthetic child, because they declare no client tools and so are not
// agents. Their spend must land on the parent: TOTAL is summed from child rows,
// so a branch nothing claims is money that silently leaves the report.
func TestCostsForRollsUnclaimedBranchesIntoTheParent(t *testing.T) {
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "66666666-6666-6666-6666-666666666666",
			ExternalRef: "c_parent", Cost: 1.0},
		{ConversationID: "77777777-7777-7777-7777-777777777777",
			ExternalRef: "c_parent:aaaa", Cost: 0.25},
		{ConversationID: "88888888-8888-8888-8888-888888888888",
			ExternalRef: "c_parent:bbbb", Cost: 0.75},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	c := &Controller{coster: cap, st: st}

	// The prefix route is what reaches those branches at all; without it the
	// query never returns them and there is nothing to attribute.
	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_parent"}})
	if !slices.Contains(cap.sel.ExternalRefPrefixes, "c_parent:") {
		t.Errorf("ExternalRefPrefixes = %v, want it to carry %q",
			cap.sel.ExternalRefPrefixes, "c_parent:")
	}
	if got["c_parent"] != 2.0 {
		t.Errorf("c_parent = %v, want 2.0 (own 1.0 plus two unclaimed branches)",
			got["c_parent"])
	}
}

// A branch a real subagent DOES claim is that subagent's spend, not the
// parent's -- otherwise every Task subagent's cost is reported twice, once on
// its own row and once folded into its parent's.
func TestCostsForLeavesAClaimedBranchOnItsOwnChild(t *testing.T) {
	branch := "c_parent:cccc"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "99999999-9999-9999-9999-999999999999",
			ExternalRef: "c_parent", Cost: 1.0},
		{ConversationID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			ExternalRef: branch, Cost: 3.0},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	st.Insert(&childstore.Session{ChildID: branch, Native: true})
	c := &Controller{coster: cap, st: st}

	got := c.costsFor([]childstore.Snapshot{
		{ChildID: "c_parent"},
		{ChildID: branch},
	})
	if got["c_parent"] != 1.0 {
		t.Errorf("c_parent = %v, want 1.0: a claimed branch is its own child's spend",
			got["c_parent"])
	}
	if got[branch] != 3.0 {
		t.Errorf("%s = %v, want 3.0", branch, got[branch])
	}
}

// Claimed-ness is a property of the CHILDSTORE, never of the snapshot slice:
// snaps is routinely status-filtered, and a real subagent filtered out of the
// list must not have its cost slide onto its parent as if it were a helper.
func TestCostsForChecksClaimsAgainstTheStoreNotTheFilteredList(t *testing.T) {
	branch := "c_parent:dddd"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			ExternalRef: branch, Cost: 3.0},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	st.Insert(&childstore.Session{ChildID: branch, Native: true})
	c := &Controller{coster: cap, st: st}

	// Only the parent is listed -- the subagent exists but was filtered out.
	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_parent"}})
	if got["c_parent"] != 0 {
		t.Errorf("c_parent = %v, want 0: a filtered-out subagent still owns its branch",
			got["c_parent"])
	}
}

// No cost source means NOT KNOWN, which must leave CostUSD nil rather than
// reporting a zero the rail would then adopt.
func TestCostsForWithNoCosterIsAbsentNotZero(t *testing.T) {
	c := &Controller{}
	if got := c.costsFor([]childstore.Snapshot{{ChildID: "c1"}}); got != nil {
		t.Errorf("costsFor with no coster = %v, want nil", got)
	}
}
