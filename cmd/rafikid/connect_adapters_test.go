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

// No cost source means NOT KNOWN, which must leave CostUSD nil rather than
// reporting a zero the rail would then adopt.
func TestCostsForWithNoCosterIsAbsentNotZero(t *testing.T) {
	c := &Controller{}
	if got := c.costsFor([]childstore.Snapshot{{ChildID: "c1"}}); got != nil {
		t.Errorf("costsFor with no coster = %v, want nil", got)
	}
}
