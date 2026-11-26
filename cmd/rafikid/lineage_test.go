// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeLineageSource stands in for childstoredb.Store's lineage view without a
// database: it returns whatever rows the test hands it, or a chosen error. Every
// row carries the parent/root it was read with (lineageRow), because the walk
// crosses labels — a member with no parent label is a row beneath nothing.
type fakeLineageSource struct {
	rows []childstore.LineageRow
	err  error

	gotAncestor string
	calls       int
}

func (f *fakeLineageSource) Lineage(_ context.Context, ancestorChildID string) ([]childstore.LineageRow, error) {
	f.gotAncestor = ancestorChildID
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// lineageRow is one member with the parent/root labels a real read carries, so a
// fake source describes a tree the walk can cross. parent and root are empty for
// a top-level child.
func lineageRow(childID, parent, root, session string, closed bool) childstore.LineageRow {
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	return childstore.LineageRow{
		Member: childstore.LineageMember{ChildID: childID, SessionID: session, Closed: closed},
		Labels: labels,
	}
}

// liveLineage is a fixture lineage source over the controller's live in-memory
// store: the stand-in for the child table a unit test does not have. Every child
// the fixture inserted is in it, at any depth, and none is closed — which is
// exactly what the child table holds for a fixture that never closes a child.
//
// It is a SOURCE, not a second definition: the selector, the spend and the
// child scope all read one Lineage call, and a test that wants closed members
// or a missing row overrides it with fakeLineageSource.
func liveLineage(c *Controller) childstore.LineageSource { return liveLineageSource{c: c} }

type liveLineageSource struct{ c *Controller }

func (l liveLineageSource) Lineage(_ context.Context, childID string) ([]childstore.LineageRow, error) {
	if childID == "" {
		return nil, nil
	}
	var rows []childstore.LineageRow
	add := func(snap childstore.Snapshot) {
		rows = append(rows, childstore.LineageRow{
			Member: childstore.LineageMember{ChildID: snap.ChildID, SessionID: snap.SessionID},
			Labels: snap.Labels,
		})
	}
	if snap, ok := l.c.st.Get(childID); ok {
		add(snap)
	}
	for _, snap := range l.c.st.Descendants(childID) {
		add(snap)
	}
	return rows, nil
}

// recordingCoster records the selector it was asked for, so a test can prove a
// closed child's conversation is still in the spend boundary.
type recordingCoster struct {
	selector insights.SubtreeSelector
	spend    float64
	err      error
}

func (f *recordingCoster) UsageByConversation(_ context.Context, sel insights.SubtreeSelector) ([]insights.ConversationUsage, error) {
	f.selector = sel
	return nil, f.err
}

func (f *recordingCoster) PriceUsage([]insights.ConversationUsage) (float64, []string) {
	return f.spend, nil
}

func (f *recordingCoster) CostsByConversation(context.Context, insights.SubtreeSelector) ([]insights.ConversationCost, error) {
	return nil, nil
}

// recordingSearchBackend captures the SearchFilter the connect adapter built.
type recordingSearchBackend struct {
	agentcli.Backend

	gotFilter insights.SearchFilter
	rows      []insights.ConversationSummary
	err       error
}

func (b *recordingSearchBackend) Search(_ context.Context, _ insights.Scope, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
	b.gotFilter = f
	return b.rows, b.err
}

func countOccurrences(in []string, want string) int {
	n := 0
	for _, s := range in {
		if s == want {
			n++
		}
	}
	return n
}

// A closed child — gone from the live in-memory store, which forgets a child the
// moment it is closed — must stay in its ancestor's subtree selector through all
// three correlation routes. The database is the only source: this fixture's live
// store is empty (both children deleted), so a single definition that leans on
// live state at all comes back with nothing.
//
// Fails against any reintroduction of the live-set union: the members listed
// here are the whole answer, and the root's own row comes from its lineage row
// rather than from a store lookup.
func TestSubtreeSelectorIncludesClosedDescendants(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	c.st.Delete("c_d0")
	c.st.Delete("c_d1")
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_d0", "", "", "s-root", false),
		lineageRow("c_d1", "c_d0", "c_d0", "s-closed", true),
	}}

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "selector")
	ck.Contains(sel.ConversationIDs, "s-root", "the root's own conversation comes from its lineage row; ids = %v", sel.ConversationIDs)
	ck.Contains(sel.ConversationIDs, "s-closed", "the closed child's conversation must stay in the subtree; ids = %v", sel.ConversationIDs)
	ck.Contains(sel.ExternalRefs, "c_d1", "the closed child's external ref; refs = %v", sel.ExternalRefs)
	ck.Contains(sel.ExternalRefPrefixes, "c_d1"+threadRefSep, "the closed child's branch prefix; prefixes = %v", sel.ExternalRefPrefixes)
}

// A member must appear exactly once in each list, even though each names a row
// through three routes: a selector that named a row twice would pass it to the
// query twice, and a duplicated prefix would match it twice again.
func TestSubtreeSelectorDeduplicatesAcrossRoutes(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	c.lineage = &fakeLineageSource{rows: func() []childstore.LineageRow {
		live := lineageRow("c_d1", "c_d0", "c_d0", "s-live", false)
		live.Member.ConversationID = "s-live" // a fundi child's conversation is its session
		return []childstore.LineageRow{
			lineageRow("c_d0", "", "", "s-root", false),
			live,
		}
	}()}

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "selector")
	ck.Eq(1, countOccurrences(sel.ConversationIDs, "s-live"), "a conversation id named through two routes listed once; ids = %v", sel.ConversationIDs)
	ck.Eq(1, countOccurrences(sel.ExternalRefs, "c_d1"), "external ref listed once; refs = %v", sel.ExternalRefs)
	ck.Eq(1, countOccurrences(sel.ExternalRefPrefixes, "c_d1"+threadRefSep), "prefix listed once; prefixes = %v", sel.ExternalRefPrefixes)
}

// A nil lineage source is an ERROR, never an empty scope: a daemon that cannot
// read its lineage cannot price a subtree or bound a child's scope, and an empty
// selector reads as "spent nothing" / "sees nothing is allowed" depending on the
// caller — one of which fails OPEN on a budget.
func TestSubtreeSelectorNilLineageIsAnError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{
		ChildID: "c_root", Status: protocol.StatusIdle, StartedAt: time.Now(),
	})

	sel, err := c.subtreeSelector(context.Background(), "c_root")
	ck.Require().Error(err, "a nil lineage source must not degrade to the live set")
	ck.StrContains(err.Error(), "subtree lineage for c_root: no lineage source", "refusal names the root and the cause; got %v", err)
	ck.True(sel.ConversationIDs == nil && sel.ExternalRefs == nil && sel.ExternalRefPrefixes == nil, "no selector on error; got %+v", sel)

	// The spend path fails closed for the same reason, with a cost source in
	// place so the lineage is what it reports.
	c.coster = fakeCoster{}
	_, err = c.subtreeSpend(context.Background(), "c_root")
	ck.Require().Error(err, "subtree spend with no lineage source must fail closed")
	ck.StrContains(err.Error(), "no lineage source", "spend refusal keeps the cause; got %v", err)
}

// A lineage error is returned, wrapped, naming the root — never swallowed into
// an empty selector.
func TestSubtreeSelectorReturnsLineageErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	c.lineage = &fakeLineageSource{err: errors.New("join blew up")}

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().Error(err, "lineage error must propagate")
	ck.StrContains(err.Error(), "subtree lineage for c_d0", "wrapped message names the root; got %v", err)
	ck.StrContains(err.Error(), "join blew up", "wrapped message keeps the cause; got %v", err)
	ck.True(sel.ConversationIDs == nil && sel.ExternalRefs == nil && sel.ExternalRefPrefixes == nil, "no selector on error; got %+v", sel)
}

// A budgeted parent whose lineage cannot be read must be REFUSED at admission:
// a budget that cannot be checked is not enforced, and treating "unknown" as
// "spend is zero" would let a closed child's spend vanish from the rollup.
func TestCheckSpawnLimitsFailsClosedOnALineageError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5 })
	c.coster = &recordingCoster{spend: 0}
	c.lineage = &fakeLineageSource{err: errors.New("lineage down")}

	err := c.checkSpawnLimits(protocol.SpawnRequest{ParentChildID: "c_d0"})
	ck.Require().Error(err, "a budgeted parent with an unreadable lineage must not spawn")
	ck.StrContains(err.Error(), "could not be read", "refusal reads as the fail-closed budget message; got %v", err)
}

// Closing a child must not LOWER its ancestor's spend: the usage read still
// asks for the closed child's conversation.
func TestSubtreeSpendCountsAClosedChildsSpend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-closed" })
	c.st.Delete("c_d1")
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_d0", "", "", "s-root", false),
		lineageRow("c_d1", "c_d0", "c_d0", "s-closed", true),
	}}
	coster := &recordingCoster{spend: 1.25}
	c.coster = coster

	spent, err := c.subtreeSpend(context.Background(), "c_d0")
	ck.Require().NoError(err, "spend")
	ck.Eq(1.25, spent, "spend")
	ck.Contains(coster.selector.ConversationIDs, "s-closed", "the coster must still price the closed child's conversation; selector = %+v", coster.selector)
}

// An ownerless per-child credential whose lineage cannot be read gets an ERROR,
// never an empty-and-allowed scope — and never an owner scope either, which
// would be the other way to widen a failed lookup.
func TestChildConversationScopeLineageErrorFailsClosed(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3) // c_d0 has no owner, so the subtree arm is the one that runs
	c.lineage = &fakeLineageSource{err: errors.New("lineage down")}

	got, err := childConversationScope(childTokenCtx("c_d0"), c)
	ck.Require().Error(err, "the child-scope verb must answer an error, not an allowed scope")
	ck.Eq(insights.Scope{}, got, "a failed lookup must not widen to an owner scope")
}

// The MCP child reader binds the documented fail-closed shape — an EMPTY
// subtree selector, which denies every row — when the lineage errors.
func TestMCPChildReaderBindsAnEmptyScopeOnALineageError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3)
	c.lineage = &fakeLineageSource{err: errors.New("lineage down")}

	r := newMCPChildConversationReader(context.Background(), c, "c_d0")
	ck.True(reflect.DeepEqual(insights.ScopeSubtree(insights.SubtreeSelector{}), r.scope),
		"reader scope must be the empty (deny-all) subtree; got %+v", r.scope)
}

// The daemon adapter must map a row's ClosedAt onto the mirror's ClosedAt —
// nil (unset) for an open conversation, never a zero time.
func TestConnectSearchMapsClosedAtRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	closedAt := time.Unix(1700000000, 0)
	open := time.Unix(1690000000, 0)
	fb := &recordingSearchBackend{rows: []insights.ConversationSummary{
		{ID: "conv-closed", CreatedAt: open, ClosedAt: &closedAt},
		{ID: "conv-open", CreatedAt: open},
	}}
	a := connectConversations{c: &Controller{insights: fb}}

	rows, err := a.Search(context.Background(), connectapi.ConversationSearchFilter{})
	ck.Require().NoError(err, "search")

	ck.Require().Eq(2, len(rows), "rows")
	ck.Require().NotNil(rows[0].ClosedAt, "a closed row maps ClosedAt -> ClosedAt")
	ck.Eq(closedAt.Unix(), rows[0].ClosedAt.Unix(), "closed-at")
	ck.True(rows[1].ClosedAt == nil, "an open row leaves ClosedAt unset (nil), never a zero time; got %v", rows[1].ClosedAt)
}
