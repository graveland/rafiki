// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"slices"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// searchDebounce is how long a query, sort or state change waits before its
// ListConversations goes out. One RPC per pause in typing, never one per
// keystroke; the tick carries the gen it was armed under and a stale one is
// ignored whole.
const searchDebounce = 250 * time.Millisecond

// searchPrefetch is how close to a level's last loaded row the cursor must
// come before move() asks for that level's next page. Within one screen of
// the end is the point: close enough that the page lands before the reader
// reaches it, far enough that a two-row page is not asked for at once.
const searchPrefetch = 10

// searchPage names one ListConversations call the model wants issued. parent
// is the level to list, "" for the top; cursor continues from a previous
// page's next_cursor, "" for the first page.
type searchPage struct {
	parent string
	cursor string
}

// searchRow is one visible row: a conversation listing, the tree depth it
// renders at, and whether its fold mark is drawn. depth drives the indent and
// the width budget; folded is set by rebuild, the way rail.Fold sets Node's.
type searchRow struct {
	l      *rafikiv1.ConversationListing
	depth  int
	folded bool
}

// searchRail is the conversation-search model. It knows nothing about
// bubbletea or the network: every method that wants a fetch returns the
// request to issue, the cockpit performs it, and applyPage folds the response
// back in. That shape is what makes the paging rules testable without a
// daemon.
//
// gen is the staleness rule. Every query, sort or state change clears the
// loaded rows and bumps it, and applyPage drops any response whose gen does
// not match: a page that answered the old question must never join the new
// result set, because the two answer different filters and interleave into
// rows that mean nothing.
//
// kids is the loaded listings per level, keyed by parent id ("" = top). rows
// is the VISIBLE flatten of those levels: each listing, then its children
// when it is expanded. Collapsing removes a subtree from rows but leaves it
// in kids, so re-expanding is instant and asks for nothing; a change of
// query, sort or states clears kids too, because children loaded under the
// old filters are the wrong answer to the new one.
type searchRail struct {
	query  string
	sort   rafikiv1.ConversationSort
	states []rafikiv1.ConversationState
	gen    int

	rows     []searchRow
	expanded map[string]bool
	cursor   int

	kids    map[string][]*rafikiv1.ConversationListing
	next    map[string]string
	loading map[string]bool
}

// newSearchRail returns the rail as it opens: no query (which lists recent
// conversations), UNSPECIFIED sort, everything collapsed.
func newSearchRail() *searchRail {
	return &searchRail{
		sort:     rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED,
		expanded: map[string]bool{},
		kids:     map[string][]*rafikiv1.ConversationListing{},
		next:     map[string]string{},
		loading:  map[string]bool{},
	}
}

// reset drops everything loaded and bumps the gen. Every request in flight
// for the old result set lands stale and is dropped by applyPage.
func (s *searchRail) reset() {
	s.rows = nil
	s.expanded = map[string]bool{}
	s.kids = map[string][]*rafikiv1.ConversationListing{}
	s.next = map[string]string{}
	s.loading = map[string]bool{}
	s.cursor = 0
	s.gen++
}

// setQuery records the query. A no-op (the text did not change) changes
// nothing; a real one clears the result set and returns the top-level page
// request to issue once the caller's debounce elapses. The loading mark goes
// on now, so nothing else can start a top page while the change is pending.
func (s *searchRail) setQuery(q string) *searchPage {
	if q == s.query {
		return nil
	}
	s.query = q
	s.reset()
	s.loading[""] = true
	return &searchPage{}
}

// setSort records the row order. Same contract as setQuery: the cursor a
// previous page returned is only valid for the sort that produced it, so the
// result set goes and the gen goes with it.
func (s *searchRail) setSort(sort rafikiv1.ConversationSort) *searchPage {
	if sort == s.sort {
		return nil
	}
	s.sort = sort
	s.reset()
	s.loading[""] = true
	return &searchPage{}
}

// setStates records the lifecycle filter. Same contract as setQuery.
func (s *searchRail) setStates(states []rafikiv1.ConversationState) *searchPage {
	if slices.Equal(states, s.states) {
		return nil
	}
	s.states = slices.Clone(states)
	s.reset()
	s.loading[""] = true
	return &searchPage{}
}

// cycleSort moves to the next sort in tab order: recent, created, best
// match. RELEVANCE needs a query on the wire (the daemon refuses it without
// one), so it is skipped while the query is empty rather than asking for a
// refusal. nil when no next sort exists, which only happens on an empty
// cycle.
func (s *searchRail) cycleSort() *searchPage {
	order := []rafikiv1.ConversationSort{
		rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED,
		rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED,
		rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE,
	}
	idx := 0
	for i, sort := range order {
		if sort == s.sort {
			idx = i
			break
		}
	}
	for i := 0; i < len(order); i++ {
		idx = (idx + 1) % len(order)
		if order[idx] == rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE && s.query == "" {
			continue
		}
		return s.setSort(order[idx])
	}
	return nil
}

// resumeTop returns the first top-level page to issue when the rail opens
// with nothing loaded. A rail reopened on its previous results shows them
// again with no round trip; nil when a page is loaded or already in flight.
func (s *searchRail) resumeTop() *searchPage {
	if s.loading[""] || len(s.kids[""]) > 0 {
		return nil
	}
	s.loading[""] = true
	return &searchPage{}
}

// applyPage folds a ListConversationsResponse into the model. A response
// whose gen is stale (the query, sort or filter changed while the RPC ran)
// is dropped whole. Continuation pages append to what the level already
// holds; within one gen a level's pages are strictly first-page-then-more,
// because every request the model hands out is guarded by the same loading
// flag the response clears.
func (s *searchRail) applyPage(gen int, parent string, resp *rafikiv1.ListConversationsResponse) bool {
	if resp == nil || gen != s.gen {
		return false
	}
	delete(s.loading, parent)
	kids := s.kids[parent]
	for _, l := range resp.GetRows() {
		if l != nil {
			kids = append(kids, l)
		}
	}
	s.kids[parent] = kids
	if cur := resp.GetNextCursor(); cur != "" {
		s.next[parent] = cur
	} else {
		delete(s.next, parent)
	}
	s.rebuild()
	return true
}

// pageFailed releases a level's in-flight mark after its RPC failed, so a
// later cursor move or re-expand can try again.
func (s *searchRail) pageFailed(parent string) {
	delete(s.loading, parent)
}

// rebuild flattens the loaded levels into the visible rows, depth first: each
// listing, then, when it is expanded, its own loaded children. The cursor
// follows the row it was on, by id, so a page landing above it does not move
// the selection onto a different conversation.
func (s *searchRail) rebuild() {
	sel := ""
	if s.cursor >= 0 && s.cursor < len(s.rows) {
		sel = s.rows[s.cursor].l.GetId()
	}
	rows := make([]searchRow, 0, len(s.rows))
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, l := range s.kids[parent] {
			rows = append(rows, searchRow{l: l, depth: depth,
				folded: l.GetChildCount() > 0 && !s.expanded[l.GetId()]})
			if s.expanded[l.GetId()] {
				walk(l.GetId(), depth+1)
			}
		}
	}
	walk("", 0)
	s.rows = rows
	if sel != "" {
		for i, r := range s.rows {
			if r.l.GetId() == sel {
				s.cursor = i
				break
			}
		}
	}
	if s.cursor >= len(s.rows) {
		s.cursor = max(len(s.rows)-1, 0)
	}
}

// toggle expands or collapses one conversation. Expanding a level with
// nothing loaded returns the page request for its first page; a level
// already loaded, or already being fetched, re-expands from cache with no
// second request. Collapsing keeps the loaded children cached so re-expand
// is instant.
func (s *searchRail) toggle(id string) *searchPage {
	if s.expanded[id] {
		delete(s.expanded, id)
		s.rebuild()
		return nil
	}
	s.expanded[id] = true
	if _, loaded := s.kids[id]; loaded {
		s.rebuild()
		return nil
	}
	if s.loading[id] {
		s.rebuild()
		return nil
	}
	s.loading[id] = true
	return &searchPage{parent: id}
}

// move shifts the cursor, clamped, and asks for the cursor level's next page
// once the cursor comes within searchPrefetch rows of what that level has
// loaded. The request goes out once: a level already loading, or one whose
// last page has landed, returns nil.
func (s *searchRail) move(delta int) *searchPage {
	if len(s.rows) == 0 {
		return nil
	}
	s.cursor = min(max(s.cursor+delta, 0), len(s.rows)-1)
	parent := s.rows[s.cursor].l.GetParentConversationId()
	cur, more := s.next[parent]
	if !more || cur == "" || s.loading[parent] {
		return nil
	}
	if s.levelEnd(s.cursor)-s.cursor > searchPrefetch {
		return nil
	}
	s.loading[parent] = true
	return &searchPage{parent: parent, cursor: cur}
}

// levelEnd is the row index of the cursor level's last loaded row: the last
// visible row sharing the cursor row's parent. The cursor row is itself one
// of them, so the answer is never before the cursor.
func (s *searchRail) levelEnd(from int) int {
	parent := s.rows[from].l.GetParentConversationId()
	end := from
	for i := from; i < len(s.rows); i++ {
		if s.rows[i].l.GetParentConversationId() == parent {
			end = i
		}
	}
	return end
}

// cursorRow is the row the cursor rests on, or nil when nothing is loaded.
func (s *searchRail) cursorRow() *searchRow {
	if s.cursor < 0 || s.cursor >= len(s.rows) {
		return nil
	}
	return &s.rows[s.cursor]
}

// selectedID is the conversation the cursor rests on, "" when none.
func (s *searchRail) selectedID() string {
	if r := s.cursorRow(); r != nil {
		return r.l.GetId()
	}
	return ""
}
