// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
)

// ── the model ───────────────────────────────────────────────────────────────

// searchListing is one ConversationListing fixture: an id, a name, a child
// count and a lifecycle state.
func searchListing(id, name string, kids int32) *rafikiv1.ConversationListing {
	return &rafikiv1.ConversationListing{
		Id: id, Name: name, ChildCount: kids,
		State: rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED,
	}
}

func searchPageResp(rows ...*rafikiv1.ConversationListing) *rafikiv1.ListConversationsResponse {
	return &rafikiv1.ListConversationsResponse{Rows: rows}
}

// rowIDs is the display order, as ids: what every ordering assertion reads.
func rowIDs(rows []searchRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.l.GetId())
	}
	return out
}

func TestSearchRailApplyPageFillsTheTopLevel(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	ck.Require().Eq(0, s.gen, "fresh gen")

	req := s.setQuery("beta")
	ck.Require().True(req != nil, "a real change returns the top page request")
	ck.Eq(1, s.gen, "the change bumped the gen")
	ck.True(s.loading[""], "the pending change marks the top level in flight")

	ck.True(s.applyPage(s.gen, "", &rafikiv1.ListConversationsResponse{
		Rows:       []*rafikiv1.ConversationListing{searchListing("a", "alpha", 0), searchListing("b", "beta", 2)},
		NextCursor: "next-1",
	}), "the page applies")
	ck.EqDeep([]string{"a", "b"}, rowIDs(s.rows), "rows")
	ck.Eq("next-1", s.next[""], "the continuation cursor is kept")
	ck.False(s.loading[""], "the page cleared the in-flight mark")
}

// A page that answers an older question (the gen moved on while the RPC
// ran) is dropped whole. Appending it would interleave two result sets filtered
// and sorted differently, and rows that mean nothing.
func TestSearchRailDropsAStaleGenPage(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.setQuery("a")
	stale := s.gen
	s.setQuery("ab")
	ck.Require().Eq(stale+1, s.gen, "the second change bumped the gen")
	ck.Require().Eq(0, len(s.rows), "the change cleared the rows")

	ck.False(s.applyPage(stale, "", searchPageResp(searchListing("a", "alpha", 0))),
		"a stale page must be dropped")
	ck.Eq(0, len(s.rows), "the stale page left rows behind")
	ck.True(s.loading[""], "the newer change's page is still pending: its mark stands")

	ck.True(s.applyPage(s.gen, "", searchPageResp(searchListing("a", "alpha", 0))),
		"the current page applies")
	ck.Eq(1, len(s.rows), "rows")
}

func TestSearchRailReexpandUsesTheCacheWithoutASecondRequest(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(searchListing("top", "root", 1)))

	req := s.toggle("top")
	ck.Require().True(req != nil, "expanding an unloaded level asks for its page")
	ck.Eq("top", req.parent, "the page names the expanded conversation as its parent")
	ck.True(s.applyPage(s.gen, "top", searchPageResp(searchListing("kid", "child", 0))),
		"the child page applies")
	ck.EqDeep([]string{"top", "kid"}, rowIDs(s.rows), "children render inline")

	ck.Require().Nil(s.toggle("top"), "collapsing returns no request")
	ck.EqDeep([]string{"top"}, rowIDs(s.rows), "the collapsed subtree left the rows")

	ck.Nil(s.toggle("top"), "re-expanding a cached level must not issue a second request")
	ck.EqDeep([]string{"top", "kid"}, rowIDs(s.rows), "the cached children are back instantly")
}

// An expand whose page is still in flight must not queue a second identical
// request: two responses for one level would append its rows twice.
func TestSearchRailExpandWhileInFlightIssuesNoSecondRequest(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(searchListing("top", "root", 1)))

	ck.Require().True(s.toggle("top") != nil, "first expand asks")
	ck.Require().Nil(s.toggle("top"), "collapse")
	ck.Nil(s.toggle("top"), "re-expand while the page is still in flight asks for nothing")
	ck.True(s.applyPage(s.gen, "top", searchPageResp(searchListing("kid", "child", 0))),
		"the page applies once")
	ck.EqDeep([]string{"top", "kid"}, rowIDs(s.rows), "one page, one set of children")
}

// A continuation page lands after the parent's existing descendants, the
// whole visible subtree and not just the last row, so the tree stays depth
// first however many pages a level needed.
func TestSearchRailInsertsChildrenAfterExistingDescendants(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(
		searchListing("p", "parent", 3), searchListing("q", "next", 0)))
	ck.Require().True(s.toggle("p") != nil, "expand p")
	ck.Require().True(s.applyPage(0, "p", searchPageResp(
		searchListing("k1", "kid one", 1), searchListing("k2", "kid two", 0))),
		"p's first page applies")
	ck.Require().True(s.toggle("k1") != nil, "expand k1")
	ck.Require().True(s.applyPage(0, "k1", searchPageResp(searchListing("g1", "grandchild", 0))),
		"k1's page applies")
	ck.EqDeep([]string{"p", "k1", "g1", "k2", "q"}, rowIDs(s.rows), "rows before the continuation")

	// The continuation page lands after p's whole visible subtree: k1's
	// grandchild included, ahead of p's next sibling.
	ck.Require().True(s.applyPage(0, "p", searchPageResp(searchListing("k3", "kid three", 0))),
		"p's continuation applies")
	ck.EqDeep([]string{"p", "k1", "g1", "k2", "k3", "q"}, rowIDs(s.rows), "rows after the continuation")
}

func TestSearchRailMoveRequestsTheNextPageNearTheEnd(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	rows := make([]*rafikiv1.ConversationListing, 0, 15)
	for i := 0; i < 15; i++ {
		rows = append(rows, searchListing("r"+itoa(int64(i)), "row", 0))
	}
	s.applyPage(0, "", &rafikiv1.ListConversationsResponse{Rows: rows, NextCursor: "cur-2"})

	// Far from the end: nothing to prefetch. Fifteen rows loaded, the guard
	// is ten: index 4 is the first position within reach.
	ck.Nil(s.move(+1), "a cursor far from the level end asks for nothing")
	ck.Nil(s.move(+1), "still far")
	ck.Nil(s.move(+1), "still far")
	ck.Eq(3, s.cursor, "the cursor moved")

	req := s.move(+1)
	ck.Require().True(req != nil, "within ten rows of the end the next page is asked for")
	ck.Eq("", req.parent, "the top level is prefetched by the top cursor")
	ck.Eq("cur-2", req.cursor, "the continuation cursor")
	ck.True(s.loading[""], "the request marks the level in flight")

	ck.Nil(s.move(+1), "a level already loading asks for nothing again")

	onRow := s.rows[s.cursor].l.GetId()
	ck.Require().True(s.applyPage(s.gen, "", &rafikiv1.ListConversationsResponse{
		Rows: []*rafikiv1.ConversationListing{searchListing("r15", "row", 0)},
	}), "the continuation applies")
	ck.False(s.loading[""], "the page cleared the in-flight mark")
	ck.Eq(onRow, s.rows[s.cursor].l.GetId(), "the cursor stayed on its row")

	ck.Nil(s.move(+1), "an exhausted level asks for nothing")
}

// The prefetch is per LEVEL: a cursor deep in an expanded subtree asks for
// that subtree's own level, with that level's continuation cursor.
func TestSearchRailMovePrefetchesTheCursorRowsOwnLevel(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	kids := make([]*rafikiv1.ConversationListing, 0, 4)
	for i := 0; i < 4; i++ {
		kids = append(kids, &rafikiv1.ConversationListing{
			Id: "k" + itoa(int64(i)), Name: "kid", ParentConversationId: "p",
			State: rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED,
		})
	}
	s.applyPage(0, "", searchPageResp(searchListing("p", "parent", 4)))
	ck.Require().True(s.toggle("p") != nil, "expand p")
	ck.Require().True(s.applyPage(0, "p", &rafikiv1.ListConversationsResponse{
		Rows: kids, NextCursor: "p-2",
	}), "p's page applies")

	// p's level holds four rows, fewer than the prefetch distance, so the
	// cursor's first step into it is already within reach of its end.
	req := s.move(+1)
	ck.Require().True(req != nil, "a barely loaded level prefetches on entry")
	ck.Eq("p", req.parent, "the prefetch names the cursor row's own level")
	ck.Eq("p-2", req.cursor, "with that level's continuation cursor")
}

// The cursor follows its row across a page insert: a continuation landing
// above the selection must not move the selection onto another conversation.
func TestSearchRailSelectionStaysPutAcrossAPageInsert(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(searchListing("p", "parent", 2), searchListing("q", "next", 0)))
	ck.Require().True(s.toggle("p") != nil, "expand p")
	ck.Require().True(s.applyPage(0, "p", searchPageResp(searchListing("k1", "kid", 0))),
		"p's page applies")
	ck.Require().Nil(s.move(+1), "onto k1")
	ck.Require().Nil(s.move(+1), "onto q")
	ck.Require().Eq("q", s.selectedID(), "cursor on q")

	// p's continuation inserts INSIDE p's block, above the cursor.
	ck.Require().True(s.applyPage(0, "p", searchPageResp(searchListing("k2", "kid", 0))),
		"p's continuation applies")
	ck.EqDeep([]string{"p", "k1", "k2", "q"}, rowIDs(s.rows), "rows")
	ck.Eq("q", s.selectedID(), "the selection stayed on q")
}

func TestSearchRailCycleSortSkipsRelevanceWithoutAQuery(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, s.sort, "fresh sort")

	req := s.cycleSort()
	ck.Require().True(req != nil, "a cycle returns the top page request")
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, s.sort, "unspecified moves to created")
	ck.Eq(1, s.gen, "the cycle bumped the gen")

	req = s.cycleSort()
	ck.Require().True(req != nil, "a cycle returns the top page request")
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, s.sort,
		"relevance is skipped without a query: the daemon refuses it")

	s.setQuery("beta")
	ck.Require().Nil(s.setQuery("beta"), "a no-op change returns nothing")
	req = s.cycleSort()
	ck.Require().True(req != nil, "a cycle returns the top page request")
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, s.sort, "created")
	req = s.cycleSort()
	ck.Require().True(req != nil, "a cycle returns the top page request")
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE, s.sort,
		"with a query the cycle reaches relevance")
}

func TestSearchRailSetStatesClearsAndBumpsTheGen(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	ck.Require().True(s.applyPage(0, "", searchPageResp(searchListing("a", "alpha", 0))),
		"seed a page")
	gen := s.gen

	req := s.setStates([]rafikiv1.ConversationState{
		rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
	})
	ck.Require().True(req != nil, "a real change returns the top page request")
	ck.Eq(gen+1, s.gen, "the change bumped the gen")
	ck.Eq(0, len(s.rows), "the change cleared the rows")
	ck.EqDeep([]rafikiv1.ConversationState{
		rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
	}, s.states, "states recorded")

	ck.Nil(s.setStates([]rafikiv1.ConversationState{
		rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
	}), "a no-op change returns nothing")
}

// ── the render ──────────────────────────────────────────────────────────────

// The row grammar: fold +/-, the (n) count, the name fallbacks, and closed
// rows dimmed. The dim is asserted exactly, against the same style the render
// applies, so a wrong colour cannot hide behind a plain-text match.
func TestSearchRailRenderShowsFoldKidCountAndDimClosedRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	open := searchListing("p", "parent", 2)
	open.State = rafikiv1.ConversationState_CONVERSATION_STATE_OPEN
	closed := searchListing("q", "closed-conv", 0)
	exited := searchListing("r", "exited-conv", 1)
	exited.State = rafikiv1.ConversationState_CONVERSATION_STATE_EXITED
	s.applyPage(0, "", searchPageResp(open, closed, exited))
	ck.Require().True(s.toggle("p") != nil, "expand p")
	ck.Require().True(s.applyPage(0, "p", searchPageResp(searchListing("k", "kid", 0))),
		"p's page applies")
	ck.Require().Eq("p", s.selectedID(), "cursor on p")

	out := renderSearchRail(s.rows, "que", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "p", 60, true, time.Time{})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	ck.Require().Eq(5, len(lines), "query line plus four rows")

	ck.Eq(padTo("/ que", 60-len(" recent"))+" recent", lines[0], "query line")
	ck.Eq("▶ - parent (2) ●", ansi.Strip(lines[1]), "the open parent row: cursor, fold, count, live tag")
	ck.Eq(styleRailDim.Render("     kid"), lines[2], "the child row right under its parent, dimmed like its state")
	ck.Eq(styleRailDim.Render("   closed-conv"), lines[3], "a closed leaf renders dimmed, no fold, no count")
	ck.Eq("  + exited-conv (1)", ansi.Strip(lines[4]), "a collapsed exited row keeps its fold, its colour and its count")

	// The same rail, collapsed again: + on the parent, nothing on the leaf.
	ck.Require().Nil(s.toggle("p"), "collapse p")
	out = renderSearchRail(s.rows, "que", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "p", 60, false, time.Time{})
	lines = strings.Split(strings.TrimRight(out, "\n"), "\n")
	ck.Eq("▸ + parent (2) ●", ansi.Strip(lines[1]), "collapsed parent row, unfocused cursor")
}

// The name falls back to the linked agent's name, then to the id's first
// eight characters, so an anonymous conversation is still addressable.
func TestSearchRailRenderNameFallbacks(t *testing.T) {
	ck := assert.NewCollecting(t)
	named := searchListing("conv-1", "conversation name", 0)
	agentNamed := &rafikiv1.ConversationListing{Id: "conv-2", ChildName: "worker-1",
		State: rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED}
	bare := &rafikiv1.ConversationListing{Id: "0198abcd-ef01-2345-6789", ChildName: "",
		State: rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED}

	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(named, agentNamed, bare))
	out := renderSearchRail(s.rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "", 80, false, time.Time{})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	ck.StrContains(lines[1], "conversation name", "the conversation's name wins")
	ck.StrContains(lines[2], "worker-1", "the agent's name is the second fallback")
	ck.StrContains(lines[3], "0198abcd", "the id's first eight characters are the last fallback")
	ck.True(strings.HasSuffix(lines[0], " recent"), "the empty query line still right-aligns the sort")
}

// A leaf renders no fold and no count; a parent shows both, collapsed or not.
func TestSearchRailRenderFoldShapes(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newSearchRail()
	s.applyPage(0, "", searchPageResp(
		searchListing("leaf", "leaf", 0), searchListing("node", "node", 1)))
	out := renderSearchRail(s.rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "", 40, false, time.Time{})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	ck.NotStrContains(ansi.Strip(lines[1]), "+", "a leaf has no fold mark")
	ck.NotStrContains(ansi.Strip(lines[1]), "(", "a leaf has no count")
	ck.StrContains(ansi.Strip(lines[2]), "+ node (1)", "a collapsed parent shows + and its count")
}

func TestSearchRailQueryLineClipsTheQueryAndKeepsTheSort(t *testing.T) {
	ck := assert.NewCollecting(t)
	line := searchQueryLine("hello", "recent", 30)
	ck.Eq(30, ansi.StringWidth(line), "the line fills the width")
	ck.True(strings.HasSuffix(line, " recent"), "the sort is right-aligned")

	long := searchQueryLine(strings.Repeat("x", 40), "recent", 20)
	ck.LessOrEqual(20, ansi.StringWidth(long), "a long query clips")
	ck.True(strings.HasSuffix(long, " recent"), "the sort survives the clip")
	ck.StrContains(long, "…", "the clip is marked")
}

// The pane sizes to its content, the way railWidthFor does for the agent
// rail: the query line and the widest row set the want, the window only
// clamps.
func TestSearchRailWidthCountsRowsAndTheQueryLine(t *testing.T) {
	ck := assert.NewCollecting(t)
	rows := []searchRow{
		{l: searchListing("a", "alpha", 0)},
		{l: searchListing("b", "beta-conv", 2), depth: 2, folded: true},
	}
	ck.Eq(searchRailWidth(rows, "q", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 400, time.Time{}), searchRailWidth(rows, "q", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 2000, time.Time{}),
		"the width tracked the window; that reflows the transcript on every drag")
	ck.Eq(railMin, searchRailWidth(rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 2000, time.Time{}), "a short list sits at the floor")

	deep := []searchRow{{l: searchListing("b", "beta-conv", 2), depth: 5, folded: true}}
	ck.Greater(railMin, searchRailWidth(deep, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 2000, time.Time{}),
		"indentation counts against the width budget")

	long := []searchRow{{l: searchListing("x", strings.Repeat("x", 300), 0)}}
	got := searchRailWidth(long, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 100, time.Time{})
	ck.LessOrEqual(100*railMaxPct/100, got, "width = %d, want it clamped to %d%% of a 100-col window", got, railMaxPct)
}

// timedListing is a listing with both of its times set relative to now.
func timedListing(id string, created, last time.Duration, now time.Time) *rafikiv1.ConversationListing {
	l := searchListing(id, id, 0)
	l.CreatedAt = timestamppb.New(now.Add(-created))
	l.LastActivity = timestamppb.New(now.Add(-last))
	return l
}

// The age column shows the time the list is ordered by, so it explains the
// order on screen: creation under the created sort, last activity under
// recent and under best match, which has no time of its own.
func TestSearchAgeFollowsTheSort(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	l := timedListing("c", 3*24*time.Hour, 5*time.Minute, now)
	ck.Eq(" 5m", searchAgeTag(l, rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, now), "recent shows last activity")
	ck.Eq(" 3d", searchAgeTag(l, rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, now), "created shows creation")
	ck.Eq(" 5m", searchAgeTag(l, rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE, now), "best match shows last activity")
	ck.Eq("", searchAgeTag(searchListing("bare", "bare", 0), rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, now),
		"a listing with no time shows no age")
}

// Each unit boundary, and the switch to a date past two weeks and to a year
// for an earlier one.
func TestSearchAgeUnits(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	created := rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, " now"},
		{time.Minute, " 1m"},
		{59 * time.Minute, " 59m"},
		{time.Hour, " 1h"},
		{23 * time.Hour, " 23h"},
		{24 * time.Hour, " 1d"},
		{13 * 24 * time.Hour, " 13d"},
		{14 * 24 * time.Hour, " Nov 3"},
		{330 * 24 * time.Hour, " 2025"},
	} {
		ck.Eq(tc.want, searchAgeTag(timedListing("c", tc.ago, 0, now), created, now), "age of %v", tc.ago)
	}
}

// The age sits flush right on its row, and a pane too narrow for both keeps
// the age and clips the name.
func TestSearchRailRendersTheAgeFlushRight(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	l := timedListing("a-rather-long-conversation-name", time.Hour, 5*time.Minute, now)
	rows := []searchRow{{l: l}}

	out := renderSearchRail(rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "", 60, false, now)
	line := ansi.Strip(strings.Split(out, "\n")[1])
	ck.Eq(60, ansi.StringWidth(line), "the row does not fill the pane")
	ck.True(strings.HasSuffix(line, " 5m"), "the age is not flush right: %q", line)

	out = renderSearchRail(rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, "", 20, false, now)
	line = ansi.Strip(strings.Split(out, "\n")[1])
	ck.True(strings.HasSuffix(line, " 5m"), "a narrow pane dropped the age: %q", line)
	ck.StrContains(line, "…", "a narrow pane did not clip the name")

	ck.Eq(2+1+len("a-rather-long-conversation-name")+len(" 5m"),
		searchRailWidth(rows, "", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, 2000, now),
		"the width does not count the age column")
}

// The status line gives the browsed conversation's creation in full and how
// long it ran from there to its last activity; the year only when it is not
// this one.
func TestSearchCreatedLine(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	l := searchListing("c", "c", 0)
	l.CreatedAt = timestamppb.New(time.Date(2026, 11, 9, 10, 29, 0, 0, time.UTC))
	l.LastActivity = timestamppb.New(time.Date(2026, 11, 17, 11, 45, 0, 0, time.UTC))
	ck.Eq("created Nov 9 10:29 · ran 8d 1h", searchCreatedLine(l, now), "this year")

	l.CreatedAt = timestamppb.New(time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC))
	l.LastActivity = timestamppb.New(time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC))
	ck.Eq("created Dec 31 2025 23:00 · ran 3h 30m", searchCreatedLine(l, now), "an earlier year names it")

	l.LastActivity = nil
	ck.Eq("created Dec 31 2025 23:00", searchCreatedLine(l, now), "no activity, no span")
	ck.Eq("", searchCreatedLine(searchListing("bare", "bare", 0), now), "no creation time, nothing")

	ck.Eq("<1m", searchSpan(20*time.Second), "under a minute")
	ck.Eq("4m", searchSpan(4*time.Minute+10*time.Second), "minutes")
}

// ── the cockpit wiring ──────────────────────────────────────────────────────

// searchFakeClient is the ControlClient the search-rail cockpit tests run
// against. Only the calls the search rail and a commit reach are implemented;
// any other call lands on the embedded nil interface and panics, which is the
// loud answer to an RPC a test did not set up.
type searchFakeClient struct {
	rafikiv1connect.ControlClient

	mu         sync.Mutex
	listReqs   []*rafikiv1.ListConversationsRequest
	histReqs   []*rafikiv1.GetHistoryRequest
	resumeReqs []*rafikiv1.ResumeRequest
	listErr    error
	histErr    error
	// pages maps "parent|cursor" to the canned response; a miss is an empty
	// page, which is also how a level reads as exhausted.
	pages   map[string]*rafikiv1.ListConversationsResponse
	history map[string][]*rafikiv1.Event
}

func (f *searchFakeClient) ListConversations(_ context.Context, req *connect.Request[rafikiv1.ListConversationsRequest]) (*connect.Response[rafikiv1.ListConversationsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listReqs = append(f.listReqs, req.Msg)
	if f.listErr != nil {
		return nil, f.listErr
	}
	key := req.Msg.GetParentConversationId() + "|" + req.Msg.GetCursor()
	return connect.NewResponse(f.pages[key]), nil
}

func (f *searchFakeClient) GetHistory(_ context.Context, req *connect.Request[rafikiv1.GetHistoryRequest]) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.histReqs = append(f.histReqs, req.Msg)
	if f.histErr != nil {
		return nil, f.histErr
	}
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: f.history[req.Msg.GetConversationId()]}), nil
}

func (f *searchFakeClient) Resume(_ context.Context, req *connect.Request[rafikiv1.ResumeRequest]) (*connect.Response[rafikiv1.ResumeResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeReqs = append(f.resumeReqs, req.Msg)
	return connect.NewResponse(&rafikiv1.ResumeResponse{}), nil
}

func (f *searchFakeClient) ListTasks(_ context.Context, _ *connect.Request[rafikiv1.ListTasksRequest]) (*connect.Response[rafikiv1.ListTasksResponse], error) {
	return connect.NewResponse(&rafikiv1.ListTasksResponse{}), nil
}

func (f *searchFakeClient) listCalls() []*rafikiv1.ListConversationsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.ListConversationsRequest(nil), f.listReqs...)
}

func (f *searchFakeClient) histCalls() []*rafikiv1.GetHistoryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.GetHistoryRequest(nil), f.histReqs...)
}

func (f *searchFakeClient) resumeCalls() []*rafikiv1.ResumeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.ResumeRequest(nil), f.resumeReqs...)
}

// searchCockpit is a cockpit with the search rail open, against a fake
// client, at a size where the rail draws. The open's first page is issued and
// folded back in the way the program loop does, so every test starts on a
// loaded rail.
func searchCockpit(t *testing.T, client *searchFakeClient) *Cockpit {
	t.Helper()
	c := newTestCockpit("")
	c.client = client
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) // ^S opens search
	searchRun(t, c, cmd)
	ck := assert.NewAborting(t)
	ck.Require().True(c.searchOpen, "search opened")
	ck.Require().Eq(1, len(client.listCalls()), "the open fetched the first page")
	return c
}

// searchRun runs a command the way the program's command goroutine would:
// batches are split, every produced message is folded back into the cockpit.
func searchRun(t *testing.T, c *Cockpit, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch m := cmd().(type) {
	case tea.BatchMsg:
		for _, sub := range m {
			searchRun(t, c, sub)
		}
	default:
		c.Update(m)
	}
}

// searchTickNow runs the query debounce by hand: the tick a change armed is
// constructed with the gen it was armed under and fed to Update, the way
// bubbletea would deliver it. Only a change still pending (rows cleared, top
// page not yet landed) acts.
func searchTickNow(t *testing.T, c *Cockpit) {
	t.Helper()
	_, cmd := c.Update(searchTickMsg{gen: c.search.gen, req: searchPage{}})
	searchRun(t, c, cmd)
}

// searchPreviewNow runs the preview debounce the same way.
func searchPreviewNow(t *testing.T, c *Cockpit) {
	t.Helper()
	_, cmd := c.Update(searchPreviewTickMsg{seq: c.searchSeq})
	searchRun(t, c, cmd)
}

// Typing edits the query; one debounced tick issues one request per change,
// and a tick armed under an older gen is ignored whole.
func TestSearchRailTypingEditsTheQueryAndDebounces(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{}}
	c := searchCockpit(t, fake)
	ck.Require().Eq(0, len(c.search.rows), "the empty page loaded nothing")

	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	c.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	c.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	ck.Eq("bet", c.search.query, "typed text reached the query")
	ck.Eq(1, len(fake.listCalls()), "typing issued nothing yet: the change waits for its debounce")

	searchTickNow(t, c)
	calls := fake.listCalls()
	ck.Require().Eq(2, len(calls), "the tick issued one request")
	ck.Eq("bet", calls[1].GetQuery(), "the request carries the query")

	// A newer change makes the older tick stale: it must be ignored whole.
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	staleGen := c.search.gen - 1
	c.Update(searchTickMsg{gen: staleGen, req: searchPage{}})
	ck.Eq(2, len(fake.listCalls()), "a stale tick issued nothing")
	ck.Eq("beta", c.search.query, "the query stands")

	searchTickNow(t, c)
	ck.Eq(3, len(fake.listCalls()), "the current tick issued the request")
	ck.Eq("beta", fake.listCalls()[2].GetQuery(), "the request carries the query")
}

// Backspace edits the query too, and an empty query has nothing to drop.
func TestSearchRailBackspaceEditsTheQuery(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{}}
	c := searchCockpit(t, fake)
	c.search.query = "ab"
	c.search.gen = 5

	c.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	ck.Eq("a", c.search.query, "backspace dropped the last rune")

	c.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	ck.Eq("", c.search.query, "backspace on the last rune")
	c.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	ck.Eq("", c.search.query, "backspace on an empty query changes nothing")

	searchTickNow(t, c)
	calls := fake.listCalls()
	ck.Require().Eq(2, len(calls), "open + one edit")
	ck.Eq("", calls[1].GetQuery(), "the request carries the empty query")
}

// Tab cycles the sort; the cycle's request waits for the same debounce, and
// a tick from before the cycle is stale.
func TestSearchRailTabCyclesTheSort(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{}}
	c := searchCockpit(t, fake)

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, c.search.sort, "tab moved to created")
	ck.Eq(1, len(fake.listCalls()), "the cycle issued nothing yet")

	searchTickNow(t, c)
	calls := fake.listCalls()
	ck.Require().Eq(2, len(calls), "the tick issued the request")
	ck.Eq(rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, calls[1].GetSort(),
		"the request carries the sort")

	c.Update(searchTickMsg{gen: c.search.gen - 1, req: searchPage{}})
	ck.Eq(2, len(fake.listCalls()), "a stale tick issued nothing")
}

// Moving the cursor onto a row fetches that row's conversation by its own id
// and renders it read-only in the transcript pane. A NEW rest re-fetches, so a
// conversation whose agent is still working is not frozen at its first view;
// what the debounce buys is one fetch per rest, not one per row swept past.
func TestSearchRailPreviewFetchesTheBrowsedConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{
		pages: map[string]*rafikiv1.ListConversationsResponse{
			"|": searchPageResp(searchListing("conv-1", "first", 0), searchListing("conv-2", "second", 0)),
		},
		history: map[string][]*rafikiv1.Event{
			// A conversation-keyed read carries no child identity on its
			// events, which is what lets them apply into a session keyed by
			// the conversation.
			"conv-2": {{Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
				Content: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{
					Text: &rafikiv1.TextBlock{Text: "hello from conv-2"},
				}}},
			}}}},
		},
	}
	c := searchCockpit(t, fake)
	ck.Require().Eq("conv-1", c.searchConv, "the open parked the cursor on the first row")

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	ck.Eq("conv-2", c.searchConv, "the cursor moved onto the second row")
	ck.Eq(0, len(fake.histCalls()), "the fetch waits for the preview debounce")

	searchPreviewNow(t, c)
	calls := fake.histCalls()
	ck.Require().Eq(1, len(calls), "the rested cursor fetched once")
	ck.Eq("conv-2", calls[0].GetConversationId(), "the fetch is conversation-keyed")
	ck.Eq("", calls[0].GetChildId(), "no child id rides a conversation-keyed fetch")

	s := c.sessions["conv-2"]
	ck.Require().True(s != nil, "the transcript landed in a conversation-keyed session")
	ck.Eq(1, len(s.Blocks), "the event applied")

	// Resting on conv-1 fetches it. Coming back to conv-2 is a new rest, so it
	// fetches again: the transcript already in hand was the initial paint.
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	searchPreviewNow(t, c)
	ck.Eq(2, len(fake.histCalls()), "conv-1 warmed once")
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)
	ck.Eq(3, len(fake.histCalls()), "the new rest on conv-2 refreshed it")

	// A sweep arms one tick per row and only the final rest's is current, so it
	// costs one fetch, not one per row it passes.
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)
	ck.Eq(4, len(fake.histCalls()), "a sweep past rows costs one rest, not one row")
}

// A stale preview tick (the cursor moved on between arm and fire) fetches
// nothing, and neither does a tick for a rail that has closed.
func TestSearchRailPreviewIgnoresAStaleSeq(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("conv-1", "first", 0), searchListing("conv-2", "second", 0)),
	}}
	c := searchCockpit(t, fake)

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	c.Update(searchPreviewTickMsg{seq: c.searchSeq - 1})
	ck.Eq(0, len(fake.histCalls()), "a stale tick fetched nothing")

	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	seq := c.searchSeq
	c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) // close search
	ck.Require().False(c.searchOpen, "search closed")
	c.Update(searchPreviewTickMsg{seq: seq})
	ck.Eq(0, len(fake.histCalls()), "a tick for a closed rail fetched nothing")
}

// ⏎ on a row whose linked agent is open commits to it: hop makes it the
// viewed and typed-to agent, and search mode ends.
func TestSearchRailEnterCommitsToALiveChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	live := searchListing("conv-live", "live work", 0)
	live.ChildId = "c_live"
	live.State = rafikiv1.ConversationState_CONVERSATION_STATE_OPEN
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(live, searchListing("conv-closed", "old work", 0)),
	}}
	c := searchCockpit(t, fake)
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_live", "live work", 0)})

	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.False(c.searchOpen, "search closed on the commit")
	ck.Eq("c_live", c.selected, "the rail cursor followed the commit")
	ck.Eq("c_live", c.focused(), "the agent is focused")
	ck.Eq(0, len(fake.resumeCalls()), "a live agent needs no resume")
}

// ⏎ on a row whose linked agent has exited resumes it and then focuses it,
// through the same resume and hop paths the agent rail uses.
func TestSearchRailEnterResumesAnExitedChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	exited := searchListing("conv-exited", "done work", 0)
	exited.ChildId = "c_exit"
	exited.State = rafikiv1.ConversationState_CONVERSATION_STATE_EXITED
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(exited),
	}}
	c := searchCockpit(t, fake)
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_exit", "done work", 0)})
	exitChild(c, "c_exit")

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.False(c.searchOpen, "search closed on the commit")
	ck.Eq("c_exit", c.selected, "the rail cursor followed the commit")
	ck.Require().Eq(1, len(fake.resumeCalls()), "the exited agent was resumed")
	ck.Eq("c_exit", fake.resumeCalls()[0].GetChildId(), "resume named the linked agent")
}

// → opens a row's child conversations and ← folds it, the agent rail's own
// fold keys. The first open asks for the children; a reopen is served from
// what was already loaded.
func TestSearchRailRightOpensAndLeftFoldsARow(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": {
			Rows: []*rafikiv1.ConversationListing{searchListing("p", "parent", 2)},
		},
		"p|": searchPageResp(searchListing("k", "kid", 0)),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	ck.True(c.searchOpen, "search stays open")
	calls := fake.listCalls()
	ck.Require().Eq(2, len(calls), "the expansion asked for the children")
	ck.Eq("p", calls[1].GetParentConversationId(), "of the expanded row")
	ck.EqDeep([]string{"p", "k"}, rowIDs(c.search.rows), "children inline")

	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.EqDeep([]string{"p"}, rowIDs(c.search.rows), "← did not fold the open row")

	_, cmd = c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	ck.EqDeep([]string{"p", "k"}, rowIDs(c.search.rows), "→ did not reopen the row")
	ck.Eq(2, len(fake.listCalls()), "the reopen asked again for children already loaded")
}

// ← on a row that is not open climbs to its parent, as it does in the agent
// rail, and the browsed transcript follows the cursor there.
func TestSearchRailLeftClimbsToTheParent(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":  {Rows: []*rafikiv1.ConversationListing{searchListing("p", "parent", 1)}},
		"p|": searchPageResp(searchListing("k", "kid", 0)),
	}}
	c := searchCockpit(t, fake)
	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	ck.Require().Eq("k", c.search.selectedID(), "setup: the cursor rests on the child")

	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.Eq("p", c.search.selectedID(), "← on a child row did not climb to its parent")
	ck.EqDeep([]string{"p", "k"}, rowIDs(c.search.rows), "climbing folded the parent")
	ck.Eq("p", c.searchConv, "the browsed transcript did not follow the climb")

	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.EqDeep([]string{"p"}, rowIDs(c.search.rows), "the next ← did not fold the parent")
}

// → on a row with no child conversations has nothing to open: it asks the
// daemon for nothing and marks nothing open.
func TestSearchRailRightOnAChildlessRowAsksForNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("solo", "solo", 0)),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	ck.Eq(1, len(fake.listCalls()), "→ on a childless row asked the daemon for its children")
	ck.False(c.search.expanded["solo"], "→ marked a childless row open")
}

// ⏎ on a conversation with no agent to switch to (here a closed one) cannot
// hop anywhere, so it hands the keys to the read-only transcript: search stays
// open, nothing is fetched or resumed, and esc gives the keys back.
func TestSearchRailEnterReadsAClosedConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	closed := searchListing("conv-closed", "old work", 0)
	closed.ChildId = "c_gone"
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(closed),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.True(c.searchOpen, "search closed: there is no agent to switch to")
	ck.True(c.searchReading, "⏎ did not hand the keys to the transcript")
	ck.Eq(1, len(fake.listCalls()), "⏎ fetched rows")
	ck.Eq(0, len(fake.resumeCalls()), "something was resumed")

	c.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	ck.False(c.searchReading, "esc did not give the keys back to the list")
	ck.True(c.searchOpen, "esc from the transcript left search instead of returning to the list")
}

// A page that fails releases the level's in-flight mark, so a later move can
// try again instead of the level being stuck unloaded forever.
func TestSearchRailFailedPageReleasesTheLevel(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{listErr: errors.New("the daemon is down")}
	c := searchCockpit(t, fake)
	ck.Require().False(c.search.loading[""], "the open's failure released the level")
	ck.NotEq("", c.notice, "the failure named itself in the notice")

	c.Update(searchPageMsg{gen: c.search.gen, parent: "some-parent",
		err: errors.New("the daemon is down")})
	ck.NotStrContains(c.notice, "some-parent", "the notice is a sentence, not a dump")
}

// searchFromRail is a cockpit in BROWSE (two live children, the first under
// the agent rail's cursor) with the search rail opened on top of it, which is
// the origin every mode invariant has to hold from.
func searchFromRail(t *testing.T, fake *searchFakeClient) *Cockpit {
	t.Helper()
	c := newTestCockpit("")
	c.client = fake
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_live", "live work", 0), summaryFor("c_other", "other work", 0),
	})
	c.focus = focusRail
	c.selected = "c_live"
	if c.railCols() == 0 {
		t.Fatalf("setup: the agent rail does not draw, so BROWSE is not the origin")
	}
	_, cmd := c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	searchRun(t, c, cmd)
	assert.NewAborting(t).Eq(focusRail, c.searchFrom, "search opened from the agent rail")
	return c
}

// searchLiveRow is the listing a commit needs: a conversation whose linked
// agent can be acted on, with no child conversations under it.
func searchLiveRow(convID, childID, name string) *rafikiv1.ConversationListing {
	l := searchListing(convID, name, 0)
	l.ChildId = childID
	l.State = rafikiv1.ConversationState_CONVERSATION_STATE_OPEN
	return l
}

// A failure that belongs to an OLDER gen must not release its level: the level
// is not stuck, it has a newer request in flight, and releasing the mark lets a
// reopen issue an undebounced top page that the pending tick then issues again,
// so every row renders twice. It must not flash a notice for a query nobody is
// asking either.
func TestSearchRailStaleFailureReleasesNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("conv-1", "first", 0)),
	}}
	c := searchCockpit(t, fake)

	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	stale := c.search.gen - 1
	ck.Require().True(c.search.loading[""], "setup: the change put a top page in flight")

	c.Update(searchPageMsg{gen: stale, parent: "", err: errors.New("the daemon is down")})
	ck.True(c.search.loading[""],
		"a stale failure released the level, so a reopen can issue a second top page")
	ck.Eq("", c.notice, "a stale failure flashed a notice for a query nobody is asking")

	// Close and reopen inside the debounce: the reopen must issue nothing, and
	// the pending tick must issue the level's page exactly once.
	c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	searchRun(t, c, cmd)
	ck.Eq(1, len(fake.listCalls()), "the reopen issued a top page that was already in flight")
	searchTickNow(t, c)
	ck.Eq(2, len(fake.listCalls()), "the pending tick issued the level's page once")

	// A failure of the CURRENT gen is the real thing: it releases the level and
	// names itself so a later move can retry.
	c.Update(searchPageMsg{gen: c.search.gen, parent: "", err: errors.New("still down")})
	ck.False(c.search.loading[""], "a current failure must release the level")
	ck.NotEq("", c.notice, "a current failure names itself in the notice")
}

// Closing search inside the preview debounce orphans the tick, so REOPENING on
// the same row must re-arm it: otherwise searchConv still matches the row, no
// fetch is ever issued, and the pane reads "No messages yet." until the cursor
// moves. The same re-arm is what retries a fetch that failed.
func TestSearchRailReopenRefetchesThePreview(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{
		pages: map[string]*rafikiv1.ListConversationsResponse{
			"|": searchPageResp(searchListing("conv-1", "first", 0), searchListing("conv-2", "second", 0)),
		},
		history: map[string][]*rafikiv1.Event{
			"conv-2": {historyEventForConv("conv-2", "hello from conv-2")},
		},
	}
	c := searchCockpit(t, fake)

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // arms the preview for conv-2
	ck.Require().Eq("conv-2", c.searchConv, "setup: the cursor rests on conv-2")

	c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) // close inside the debounce
	ck.Require().False(c.searchOpen, "search closed")
	afterClose := c.searchSeq

	c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	ck.NotEq(afterClose, c.searchSeq,
		"the reopen armed no preview for the row under the cursor")
	searchPreviewNow(t, c)
	ck.Require().Eq(1, len(fake.histCalls()), "the reopened pane fetched nothing")
	ck.Eq("conv-2", fake.histCalls()[0].GetConversationId(), "the reopen fetched the row it opened on")
	ck.StrContains(c.View().Content, "hello from conv-2", "the reopened pane shows the transcript")
}

// A query change clears the browsed transcript with the rows it described: the
// pane showed one result set's conversation beside the next query's empty rail
// for the whole debounce plus RPC.
func TestSearchRailQueryChangeDropsTheBrowsedTranscript(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{
		pages: map[string]*rafikiv1.ListConversationsResponse{
			"|": searchPageResp(searchListing("conv-1", "first", 0)),
		},
		history: map[string][]*rafikiv1.Event{
			"conv-1": {historyEventForConv("conv-1", "hello from conv-1")},
		},
	}
	c := searchCockpit(t, fake)
	searchPreviewNow(t, c)
	ck.Require().StrContains(c.View().Content, "hello from conv-1", "setup: the transcript is on screen")

	c.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	ck.Eq("", c.searchConv, "the reset kept the previous result's conversation under the cursor")
	ck.NotStrContains(c.View().Content, "hello from conv-1",
		"the pane still showed the previous result's transcript")
}

// A failed CHILD page must collapse the row again: left expanded it claims to
// have children it never got, and the next → finds it already open and
// retries nothing.
func TestSearchRailFailedChildPageCollapsesTheRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":  {Rows: []*rafikiv1.ConversationListing{searchListing("p", "parent", 2)}},
		"p|": searchPageResp(searchListing("k", "kid", 0)),
	}}
	c := searchCockpit(t, fake)

	// The expansion's request is never run, so its failure is the only thing
	// that answers it.
	c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	ck.Require().True(c.search.expanded["p"], "setup: → expanded the row")
	ck.Require().Eq(1, len(fake.listCalls()), "setup: the expansion asked for its children")

	c.Update(searchPageMsg{gen: c.search.gen, parent: "p", err: errors.New("the daemon is down")})
	ck.False(c.search.expanded["p"], "a failed child page left the row claiming to be expanded")
	ck.False(c.search.loading["p"], "the failed level stayed in flight, so nothing can retry")

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	ck.Eq(2, len(fake.listCalls()), "the next → did not retry")
	ck.EqDeep([]string{"p", "k"}, rowIDs(c.search.rows), "the retry's children landed under the row")
}

// A preview fetch that failed is retried on the next rest of the same row: the
// failure leaves no session behind, and the only thing between the reader and a
// second try used to be the "already fetched" early-out on the same row.
func TestSearchRailFailedPreviewIsRetriedOnTheNextRest(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{
		pages: map[string]*rafikiv1.ListConversationsResponse{
			"|": searchPageResp(searchListing("conv-1", "first", 0)),
		},
		histErr: errors.New("the daemon is down"),
	}
	c := searchCockpit(t, fake)
	searchPreviewNow(t, c)
	ck.Require().Eq(1, len(fake.histCalls()), "setup: the first rest fetched")
	ck.False(c.searchHistInFlight["conv-1"], "the failure did not release the in-flight mark")
	ck.Nil(c.sessions["conv-1"], "setup: the failed read left no transcript")

	// Resting on the same row again (the cursor is already on it, so the move is
	// clamped) must try again rather than serve the absence from memory. The
	// armed seq is the assertion: without it no tick exists to fire.
	fake.mu.Lock()
	fake.histErr = nil
	fake.history = map[string][]*rafikiv1.Event{
		"conv-1": {historyEventForConv("conv-1", "second time lucky")},
	}
	fake.mu.Unlock()
	armed := c.searchSeq
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	ck.NotEq(armed, c.searchSeq, "the next rest armed no preview for the row it is already on")
	searchPreviewNow(t, c)
	ck.Require().Eq(2, len(fake.histCalls()), "the next rest did not retry the failed fetch")
	ck.Require().True(c.sessions["conv-1"] != nil, "the retry left no transcript")
	ck.Eq(1, len(c.sessions["conv-1"].Blocks), "the retry's transcript landed")
}

// A failed transcript fetch says what the daemon said, through the same trim
// every page error uses, rather than connect's transport code.
func TestSearchRailHistoryFailureNamesTheDaemonsSentence(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("conv-1", "first", 0)),
	}}
	c := searchCockpit(t, fake)
	searchPreviewNow(t, c)

	c.Update(searchHistoryMsg{convID: "conv-1",
		err: connect.NewError(connect.CodeInternal, errors.New("conversation is gone"))})
	ck.False(c.searchHistInFlight["conv-1"], "the failure did not release the in-flight mark")
	ck.Eq("history unavailable: conversation is gone", c.notice,
		"the notice is the daemon's sentence, not connect's transport code")
}

// ⏎ switches to a row's agent even when the row has child conversations:
// opening them is →'s job alone, so ⏎ has one meaning. An exited agent is
// resumed on the way, and its child conversations are not fetched.
func TestSearchRailEnterSwitchesToARowWithChildren(t *testing.T) {
	ck := assert.NewCollecting(t)
	exited := searchListing("conv-exited", "done work", 2)
	exited.ChildId = "c_exit"
	exited.State = rafikiv1.ConversationState_CONVERSATION_STATE_EXITED
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":            searchPageResp(exited),
		"conv-exited|": searchPageResp(searchListing("kid", "subagent conversation", 0)),
	}}
	c := searchCockpit(t, fake)
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_exit", "done work", 0)})
	exitChild(c, "c_exit")

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.False(c.searchOpen, "⏎ on a row with children did not switch to its agent")
	ck.Require().Eq(1, len(fake.resumeCalls()), "the switch did not resume the exited agent")
	ck.Eq("c_exit", c.selected, "the rail cursor did not follow the switch")
	ck.Eq(1, len(fake.listCalls()), "⏎ fetched the row's child conversations")
}

// → on a live row with children opens them without leaving search: browsing a
// coordinator's subagents must not hop away from the browser.
func TestSearchRailRightOpensALiveRowsChildren(t *testing.T) {
	ck := assert.NewCollecting(t)
	live := searchListing("conv-live", "live work", 1)
	live.ChildId = "c_live"
	live.State = rafikiv1.ConversationState_CONVERSATION_STATE_OPEN
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":          searchPageResp(live),
		"conv-live|": searchPageResp(searchListing("kid", "subagent conversation", 0)),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	ck.True(c.searchOpen, "→ on a live row with children left search")
	ck.Eq(focusRail, c.focus, "→ moved focus")
	ck.EqDeep([]string{"conv-live", "kid"}, rowIDs(c.search.rows), "→ did not open the row")
}

// A commit lands where the agent rail's own ⏎ lands, whatever pane search was
// opened from: that agent's transcript, keys in the input, no rail drawn beside
// them.
func TestSearchRailCommitFromTheAgentRailLandsInTheInput(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchLiveRow("conv-live", "c_live", "live work")),
	}}
	c := searchFromRail(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.False(c.searchOpen, "search closed on the commit")
	ck.Eq(focusInput, c.focus, "commit from a rail origin left focus")
	ck.Eq(0, c.railCols(), "the agent rail stayed drawn beside a focused input")
	ck.True(c.ta.Focused(), "the textarea was left blurred")
	ck.Eq("c_live", c.selected, "the rail cursor followed the commit")
}

// The cockpit holds two modes and nothing between them: BROWSE (rail drawn AND
// focused) and TALK (full width, keys in the input). Every exit from the search
// rail has to land in one of them.
func TestSearchRailEveryExitLeavesTheTwoModes(t *testing.T) {
	exits := []struct {
		name  string
		press func(c *Cockpit)
	}{
		{"esc", func(c *Cockpit) { c.Update(keyMsg("esc")) }},
		{"^S", func(c *Cockpit) { c.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) }},
		{"^R", func(c *Cockpit) { c.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}) }},
		{"shift+tab", func(c *Cockpit) { c.Update(keyMsg("shift+tab")) }},
		{"enter", func(c *Cockpit) { c.Update(keyMsg("enter")) }},
	}
	for _, tc := range exits {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
				"|": searchPageResp(searchLiveRow("conv-live", "c_live", "live work")),
			}}
			c := searchFromRail(t, fake)
			ck.Require().True(c.searchOpen, "setup: search is open")

			tc.press(c)

			ck.False(c.searchOpen, "%s left search open", tc.name)
			if c.focus == focusInput {
				ck.Eq(0, c.railCols(),
					"%s left the agent rail drawn beside a focused input", tc.name)
				ck.True(c.ta.Focused(), "%s left the input unfocused", tc.name)
				return
			}
			ck.Eq(focusRail, c.focus, "%s left focus on %v", tc.name, c.focus)
			ck.NotEq(0, c.railCols(), "%s landed in BROWSE with no rail drawn", tc.name)
		})
	}
}

// The attention hops are the agent rail's own cursor motion. Running one under
// an open search would rewrite `selected` and move the focus subscription while
// the pane draws a browsed conversation, and nothing on screen would say so.
func TestSearchRailHopKeysDoNotMoveTheAgentRail(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchLiveRow("conv-live", "c_live", "live work")),
	}}
	c := searchFromRail(t, fake)

	for _, msg := range []tea.KeyPressMsg{
		{Code: 'n', Mod: tea.ModCtrl},
		{Code: 'p', Mod: tea.ModCtrl},
		{Code: tea.KeyUp, Mod: tea.ModCtrl},
		{Code: tea.KeyDown, Mod: tea.ModCtrl},
	} {
		c.Update(msg)
		ck.Eq("c_live", c.selected, "%s moved the agent rail's cursor while search was open", msg.String())
		ck.Eq("", c.focusChild, "%s opened a focus stream for an agent nobody is looking at", msg.String())
		ck.True(c.searchOpen, "%s closed search", msg.String())
	}
	ck.Eq("", c.search.query, "a hop key was typed into the query")
}

// Displayed is read, and under an open search NOTHING agent-side is displayed:
// the rail cursor is a leftover, and a history fetch for its child must not
// clear that agent's badge or open its stream.
func TestSearchRailDoesNotReadTheAgentRailCursorsChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("conv-1", "first", 0)),
	}}
	c := searchCockpit(t, fake)
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_live", "live work", 0), summaryFor("c_other", "other work", 0),
	})
	c.rail.SetFocus("c_other")
	c.Update(railEventMsg{evs: []*rafikiv1.Event{turnEndFor("c_live", 8)}})
	n, ok := c.rail.Get("c_live")
	ck.Require().True(ok, "setup: the agent has a row")
	ck.Require().Eq(1, n.Attention, "setup: the agent carries a badge")

	// The user browsed the rail onto that agent and then opened search.
	c.focus = focusRail
	c.selected = "c_live"
	c.sessions["c_live"] = session.New("c_live")

	c.Update(historyMsg{childID: "c_live", after: 8, events: []*rafikiv1.Event{
		historyEvent("c_live", "nobody is looking at this", 0, false),
	}})
	n, _ = c.rail.Get("c_live")
	ck.Eq(1, n.Attention, "a fetch for an undisplayed agent cleared its badge")
	ck.Eq("", c.focusChild, "a fetch for an undisplayed agent opened its focus stream")
}

// In the list, PgUp/PgDn and home/end move the cursor exactly as they do in
// the agent rail, and the browsed transcript follows it.
func TestSearchRailPageKeysMoveTheCursor(t *testing.T) {
	ck := assert.NewCollecting(t)
	rows := make([]*rafikiv1.ConversationListing, 0, 100)
	for i := 0; i < 100; i++ {
		rows = append(rows, searchListing("conv-"+strconv.Itoa(i), "row", 0))
	}
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(rows...),
	}}
	c := searchCockpit(t, fake)
	page := c.railPage()
	ck.Require().Less(100, page, "setup: a page is shorter than the list")

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	ck.Eq(page, c.search.cursor, "PgDn did not move the cursor a page down")
	ck.Eq("conv-"+strconv.Itoa(page), c.searchConv, "the transcript did not follow PgDn")
	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	ck.Eq(0, c.search.cursor, "PgUp did not move the cursor a page up")
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	ck.Eq(99, c.search.cursor, "end did not move to the last row")
	c.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	ck.Eq(0, c.search.cursor, "home did not move to the first row")
	ck.False(c.searchReading, "a page key handed the keys to the transcript")
}

// Once ⏎ has handed the keys to a read-only transcript, the scroll keys move
// through it, as they do from the input pane, and leave the list's cursor
// alone; esc hands them back and the arrows move the list again.
func TestSearchRailReadingScrollsTheTranscript(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(searchListing("conv-1", "first", 0), searchListing("conv-2", "second", 0)),
	}}
	c := searchCockpit(t, fake)
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)

	s := c.sessions["conv-2"]
	ck.Require().True(s != nil, "setup: the browsed transcript loaded")
	for i := 0; i < 200; i++ {
		s.Blocks = append(s.Blocks, session.Block{Kind: session.KindSystem,
			Text: "line " + strconv.Itoa(i), Final: true})
	}
	s.Finalized = len(s.Blocks)
	p := c.pane("conv-2")
	c.View()
	p.vp.GotoBottom()
	bottom := p.vp.YOffset()
	ck.Require().Greater(0, bottom, "setup: the transcript is taller than the pane")

	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.Require().True(c.searchReading, "setup: ⏎ handed the keys to the transcript")

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	ck.Less(bottom, p.vp.YOffset(), "PgUp did not scroll the transcript")
	c.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	ck.Eq(0, p.vp.YOffset(), "home did not jump to the top")
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	ck.Eq(1, p.vp.YOffset(), "↓ did not scroll the transcript a line")
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	ck.Eq(bottom, p.vp.YOffset(), "end did not jump to the bottom")
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	ck.Eq(bottom-1, p.vp.YOffset(), "↑ did not scroll the transcript a line")
	ck.Eq("conv-2", c.search.selectedID(), "a scroll key moved the list's cursor")
	ck.True(c.searchOpen, "a scroll key left search")

	c.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	ck.False(c.searchReading, "esc did not hand the keys back to the list")
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	ck.Eq("conv-1", c.search.selectedID(), "↑ did not move the list after esc")
}

// Browsed transcripts are ordinary sessions: touched on every rest and on every
// fetch, so the LRU bounds them too. A long browsing session must not hold a
// pane and a renderer for every conversation it passed.
func TestSearchRailBrowsedSessionsAreEvicted(t *testing.T) {
	ck := assert.NewCollecting(t)
	rows := make([]*rafikiv1.ConversationListing, 0, maxSessions+2)
	for i := 0; i < maxSessions+2; i++ {
		rows = append(rows, searchListing("conv-"+strconv.Itoa(i), "browsed", 0))
	}
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": searchPageResp(rows...),
	}}
	c := searchCockpit(t, fake)
	for i := 0; i < maxSessions+2; i++ {
		if i > 0 {
			c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		searchPreviewNow(t, c)
	}
	ck.Require().Eq(maxSessions+2, len(fake.histCalls()), "setup: every row was fetched once")
	ck.LessOrEqual(maxSessions, len(c.sessions), "browsed transcripts are never evicted")
	ck.Nil(c.sessions["conv-0"], "the oldest browsed conversation survived the LRU")
}

// A second read of a conversation whose agent is still working must REPLACE the
// transcript, not append to it: ApplyHistory has no dedup, so folding a refetch
// into the session already on screen would draw every block twice.
func TestSearchRailRefetchReplacesTheTranscript(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{
		pages: map[string]*rafikiv1.ListConversationsResponse{
			"|": searchPageResp(searchListing("conv-1", "first", 0), searchListing("conv-2", "second", 0)),
		},
		history: map[string][]*rafikiv1.Event{
			"conv-2": {historyEventForConv("conv-2", "first line")},
		},
	}
	c := searchCockpit(t, fake)
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)
	ck.Require().Eq(1, len(c.sessions["conv-2"].Blocks), "setup: the first read landed")

	// The conversation grew while the cursor was away.
	fake.mu.Lock()
	fake.history["conv-2"] = []*rafikiv1.Event{
		historyEventForConv("conv-2", "first line"),
		historyEventForConv("conv-2", "second line"),
	}
	fake.mu.Unlock()
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	searchPreviewNow(t, c)
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)

	s := c.sessions["conv-2"]
	ck.Require().True(s != nil, "the refetch left no session")
	ck.Eq(2, len(s.Blocks), "the refetch appended to the transcript instead of replacing it")
}

// historyEventForConv is one conversation-keyed user message: a GetHistory
// response for a browsed conversation carries no child identity on its events.
func historyEventForConv(convID, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: convID,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{
				Text: &rafikiv1.TextBlock{Text: text},
			}}},
		}}}
}

// The search rail's width and the transcript pane render through View.
func TestSearchRailRendersThroughTheView(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":       searchPageResp(searchListing("conv-1", "first", 1)),
		"conv-1|": searchPageResp(searchListing("kid", "kid", 0)),
	}}
	c := searchCockpit(t, fake)

	v := c.View().Content
	ck.StrContains(v, "/ ", "the query line is drawn")
	ck.StrContains(v, "first (1)", "the row carries its child count")

	// Expand: the child appears under its parent.
	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	searchRun(t, c, cmd)
	v = c.View().Content
	ck.StrContains(v, "kid", "the expanded child is drawn")
}
