// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

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

	out := renderSearchRail(s.rows, "que", "recent", "p", 60, true)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	ck.Require().Eq(5, len(lines), "query line plus four rows")

	ck.Eq(padTo("/ que", 60-len(" recent"))+" recent", lines[0], "query line")
	ck.Eq("▶ - parent (2) ●", ansi.Strip(lines[1]), "the open parent row: cursor, fold, count, live tag")
	ck.Eq(styleRailDim.Render("     kid"), lines[2], "the child row right under its parent, dimmed like its state")
	ck.Eq(styleRailDim.Render("   closed-conv"), lines[3], "a closed leaf renders dimmed, no fold, no count")
	ck.Eq("  + exited-conv (1)", ansi.Strip(lines[4]), "a collapsed exited row keeps its fold, its colour and its count")

	// The same rail, collapsed again: + on the parent, nothing on the leaf.
	ck.Require().Nil(s.toggle("p"), "collapse p")
	out = renderSearchRail(s.rows, "que", "recent", "p", 60, false)
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
	out := renderSearchRail(s.rows, "", "recent", "", 80, false)
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
	out := renderSearchRail(s.rows, "", "recent", "", 40, false)
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
	ck.Eq(searchRailWidth(rows, "q", "recent", 400), searchRailWidth(rows, "q", "recent", 2000),
		"the width tracked the window; that reflows the transcript on every drag")
	ck.Eq(railMin, searchRailWidth(rows, "", "recent", 2000), "a short list sits at the floor")

	deep := []searchRow{{l: searchListing("b", "beta-conv", 2), depth: 5, folded: true}}
	ck.Greater(railMin, searchRailWidth(deep, "", "recent", 2000),
		"indentation counts against the width budget")

	long := []searchRow{{l: searchListing("x", strings.Repeat("x", 300), 0)}}
	got := searchRailWidth(long, "", "recent", 100)
	ck.LessOrEqual(100*railMaxPct/100, got, "width = %d, want it clamped to %d%% of a 100-col window", got, railMaxPct)
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
// and renders it read-only in the transcript pane; the fetch is cached, so a
// second rest on the same row costs nothing.
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

	// Resting on conv-1 fetches it; returning to conv-2 is served from cache.
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	searchPreviewNow(t, c)
	ck.Eq(2, len(fake.histCalls()), "conv-1 warmed once")
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)
	ck.Eq(2, len(fake.histCalls()), "conv-2 served from cache")

	// A sweep that ends on a cached row fetches nothing: the ticks the cursor
	// outran are stale by design, so the sweep costs one rest, not one row.
	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	searchPreviewNow(t, c)
	ck.Eq(2, len(fake.histCalls()), "a sweep past cached rows costs nothing")
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

// ⏎ on a row with no actionable agent expands or collapses it, like right
// and left do; the expansion asks for the row's children.
func TestSearchRailEnterExpandsAChildlessRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|": {
			Rows: []*rafikiv1.ConversationListing{searchListing("p", "parent", 2)},
		},
		"p|": searchPageResp(searchListing("k", "kid", 0)),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.True(c.searchOpen, "search stays open")
	calls := fake.listCalls()
	ck.Require().Eq(2, len(calls), "the expansion asked for the children")
	ck.Eq("p", calls[1].GetParentConversationId(), "of the expanded row")
	ck.EqDeep([]string{"p", "k"}, rowIDs(c.search.rows), "children inline")

	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.EqDeep([]string{"p"}, rowIDs(c.search.rows), "the second enter collapsed the row")
}

// ⏎ on a row whose linked agent is closed does not commit: the row expands.
func TestSearchRailEnterExpandsAClosedAgentsRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	closed := searchListing("conv-closed", "old work", 0)
	closed.ChildId = "c_gone"
	closed.State = rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED
	fake := &searchFakeClient{pages: map[string]*rafikiv1.ListConversationsResponse{
		"|":            searchPageResp(closed),
		"conv-closed|": searchPageResp(),
	}}
	c := searchCockpit(t, fake)

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	searchRun(t, c, cmd)
	ck.True(c.searchOpen, "search stays open: there is nothing to commit to")
	ck.Eq(2, len(fake.listCalls()), "the row expanded instead")
	ck.Eq(0, len(fake.resumeCalls()), "nothing was resumed")
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
