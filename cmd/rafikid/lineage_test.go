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
// database: it returns whatever members the test hands it, or a chosen error.
type fakeLineageSource struct {
	members []childstore.LineageMember
	err     error

	gotAncestor string
}

func (f *fakeLineageSource) Lineage(_ context.Context, ancestorChildID string) ([]childstore.LineageMember, error) {
	f.gotAncestor = ancestorChildID
	return f.members, f.err
}

// recordingCoster records the selector it was priced with, so a test can prove
// a closed child's conversation is still in the spend boundary.
type recordingCoster struct {
	selector insights.SubtreeSelector
	spend    float64
	err      error
}

func (f *recordingCoster) SubtreeCost(_ context.Context, sel insights.SubtreeSelector) (float64, error) {
	f.selector = sel
	return f.spend, f.err
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

// A closed child — deleted from the live in-memory set — must stay in its
// ancestor's subtree selector, through all three correlation routes, because
// the wired lineage source still knows about it. THIS test fails against the
// pre-change subtreeSelector (whose signature had no error and never consulted
// a lineage source): the closed child is simply gone from the live set, so
// s-closed/c_closed/c_closed: are all absent.
func TestSubtreeSelectorIncludesClosedDescendants(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-closed" })
	// The close: Controller.Close forgets live state, so the live set no longer
	// knows c_d1. Only the lineage source should still carry it.
	c.st.Delete("c_d1")
	c.lineage = &fakeLineageSource{members: []childstore.LineageMember{
		{ChildID: "c_d1", SessionID: "s-closed", Closed: true},
	}}

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "selector")
	ck.Contains(sel.ConversationIDs, "s-closed", "the closed child's conversation must stay in the subtree; ids = %v", sel.ConversationIDs)
	ck.Contains(sel.ExternalRefs, "c_d1", "the closed child's external ref; refs = %v", sel.ExternalRefs)
	ck.Contains(sel.ExternalRefPrefixes, "c_d1"+threadRefSep, "the closed child's branch prefix; prefixes = %v", sel.ExternalRefPrefixes)
}

// A live child the lineage source also returns must appear exactly once in each
// list: closing nothing, the two sources overlap by design.
func TestSubtreeSelectorDeduplicatesLiveAndLineage(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-live" })
	c.lineage = &fakeLineageSource{members: []childstore.LineageMember{
		{ChildID: "c_d1", SessionID: "s-live"},
	}}

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "selector")
	ck.Eq(1, countOccurrences(sel.ConversationIDs, "s-live"), "conversation id listed once; ids = %v", sel.ConversationIDs)
	ck.Eq(1, countOccurrences(sel.ExternalRefs, "c_d1"), "external ref listed once; refs = %v", sel.ExternalRefs)
	ck.Eq(1, countOccurrences(sel.ExternalRefPrefixes, "c_d1"+threadRefSep), "prefix listed once; prefixes = %v", sel.ExternalRefPrefixes)
}

// With no lineage source (a test, a DB-less daemon) the selector is the live
// set alone, with no error and no panic.
func TestSubtreeSelectorWithoutALineageSourceFallsBackToLiveSet(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-live" })

	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "nil source must not error")
	ck.Contains(sel.ConversationIDs, "s-live", "live descendant; ids = %v", sel.ConversationIDs)
	ck.Contains(sel.ExternalRefs, "c_d1", "live descendant ref; refs = %v", sel.ExternalRefs)
	ck.Contains(sel.ExternalRefPrefixes, "c_d1"+threadRefSep, "live descendant prefix; prefixes = %v", sel.ExternalRefPrefixes)
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

// Closing a child must not LOWER its ancestor's spend: the selector handed to
// the coster still names the closed child's conversation.
func TestSubtreeSpendCountsAClosedChildsSpend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-closed" })
	c.st.Delete("c_d1")
	c.lineage = &fakeLineageSource{members: []childstore.LineageMember{
		{ChildID: "c_d1", SessionID: "s-closed", Closed: true},
	}}
	coster := &recordingCoster{spend: 1.25}
	c.coster = coster

	spent, err := c.subtreeSpend(context.Background(), "c_d0")
	ck.Require().NoError(err, "spend")
	ck.Eq(1.25, spent, "spend")
	ck.Contains(coster.selector.ConversationIDs, "s-closed", "the coster must still price the closed child's conversation; selector = %+v", coster.selector)
}

// A per-child credential whose lineage cannot be read gets an ERROR, never an
// empty-and-allowed scope.
func TestChildConversationScopeDeniesOnALineageError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3)
	c.lineage = &fakeLineageSource{err: errors.New("lineage down")}

	_, err := childConversationScope(childTokenCtx("c_d0"), c)
	ck.Require().Error(err, "the child-scope verb must answer an error, not an allowed scope")
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
