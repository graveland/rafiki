// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"
)

// listExcluded mirrors recall.ExcludedEntrypoints without importing pkg/recall:
// pkg/insights stays free of that dependency, and every caller passes the list
// the way these tests do.
var listExcluded = []string{"recall-summary", "analyze"}

// listOwnerUser creates the fixture owner: a conversations.users row with a
// per-run unique name, returning the id a ScopeOwner bounds listings with.
func listOwnerUser(t *testing.T, pool *pgxpool.Pool, label string) string {
	t.Helper()
	return ensureUser(t, pool, fmt.Sprintf("%s-%d", label, time.Now().UnixNano()))
}

// listConvSeed is one conversations.conversation fixture row.
type listConvSeed struct {
	ownerID   string    // users.id; "" leaves owner_user_id NULL
	name      string    // NULL when empty
	model     string    // NULL when empty
	entry     string    // origin_entrypoint; 'test' when empty
	ref       string    // external_ref; NULL when empty
	cwd       string    // NULL when empty
	nodeCnt   *int      // node_count; 1 when nil
	forkFrom  string    // conversation_message id; NULL when empty
	parentID  string    // parent_conversation_id; NULL when empty
	createdAt time.Time // now() when zero
	updatedAt time.Time // now() when zero
	closedAt  *time.Time
}

func listConversation(t *testing.T, pool *pgxpool.Pool, s listConvSeed) string {
	t.Helper()
	var ownerArg, forkArg, parentArg any
	if s.ownerID != "" {
		ownerArg = s.ownerID
	}
	if s.forkFrom != "" {
		forkArg = s.forkFrom
	}
	if s.parentID != "" {
		parentArg = s.parentID
	}
	entry := s.entry
	if entry == "" {
		entry = "test"
	}
	nodeCnt := 1
	if s.nodeCnt != nil {
		nodeCnt = *s.nodeCnt
	}
	created := s.createdAt
	if created.IsZero() {
		created = time.Now()
	}
	updated := s.updatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	var id string
	err := pool.QueryRow(context.Background(), `
INSERT INTO conversations.conversation
    (owner_user_id, name, model, origin_entrypoint, driven_by, external_ref, cwd,
     node_count, fork_from, parent_conversation_id, created_at, updated_at, closed_at)
VALUES ($1::uuid, NULLIF($2,''), NULLIF($3,''), $4, 'client', NULLIF($5,''), NULLIF($6,''),
        $7, $8::uuid, $9::uuid, $10, $11, $12)
RETURNING id::text`,
		ownerArg, s.name, s.model, entry, s.ref, s.cwd, nodeCnt, forkArg, parentArg,
		created, updated, s.closedAt).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation fixture")
	return id
}

// listChildSeed is one conversations.child fixture row.
type listChildSeed struct {
	childID   string
	convID    string // conversation_id; NULL when empty (a claude-style child)
	kind      string
	status    string // status column; 'idle' when empty
	spawnedAt time.Time
	name      string // NULL when empty
	cwd       string // NULL when empty
	exitedAt  *time.Time
	closedAt  *time.Time
	labels    map[string]string
}

func listChild(t *testing.T, pool *pgxpool.Pool, s listChildSeed) {
	t.Helper()
	var convArg any
	if s.convID != "" {
		convArg = s.convID
	}
	status := s.status
	if status == "" {
		status = "idle"
	}
	spawnedAt := s.spawnedAt
	if spawnedAt.IsZero() {
		spawnedAt = time.Now()
	}
	labels := "{}"
	if len(s.labels) > 0 {
		b, err := json.Marshal(s.labels)
		assert.NewAborting(t).NoError(err, "marshal labels")
		labels = string(b)
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO conversations.child
    (child_id, conversation_id, kind, status, spawned_at, name, cwd, exited_at, closed_at, labels)
VALUES ($1, $2::uuid, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), $8, $9, $10::jsonb)`,
		s.childID, convArg, s.kind, status, spawnedAt, s.name, s.cwd, s.exitedAt, s.closedAt, labels)
	assert.NewAborting(t).NoError(err, "insert child fixture")
}

// listWindow inserts one conversation_window row, the text a Query matches.
func listWindow(t *testing.T, pool *pgxpool.Pool, convID, ownerID, text string, seq int) {
	t.Helper()
	var ownerArg any
	if ownerID != "" {
		ownerArg = ownerID
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO conversations.conversation_window
    (conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
VALUES ($1::uuid, $2::uuid, $3, 0, 5, $4, false, 1)`,
		convID, ownerArg, seq, text)
	assert.NewAborting(t).NoError(err, "insert conversation_window fixture")
}

// listForkSource inserts a message into convID and returns its id, the value a
// fork_from column needs (it references conversation_message).
func listForkSource(t *testing.T, pool *pgxpool.Pool, convID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
VALUES ($1::uuid, 999, 'user', '"seed"') RETURNING id::text`, convID).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert fork source message")
	return id
}

func listPricing(t *testing.T, pool *pgxpool.Pool, model string, prompt, completion, cacheRead float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO conversations.model_pricing (model_id, prompt_usd, completion_usd, cache_read_usd)
VALUES ($1, $2, $3, $4)`, model, prompt, completion, cacheRead)
	assert.NewAborting(t).NoError(err, "insert model_pricing fixture")
}

// listBM25Ready reports whether this database carries the BM25 index a Query
// needs. Recall search installs it with pg_textsearch; tests that exercise the
// query itself skip without it.
func listBM25Ready(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	err := pool.QueryRow(context.Background(),
		"SELECT to_regclass('conversations.conversation_window_bm25') IS NOT NULL").Scan(&ok)
	assert.NewAborting(t).NoError(err, "probe conversation_window_bm25")
	return ok
}

// listIDs collects one listing into id strings.
func listIDs(rows []ConversationListing) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// listByID indexes one listing by conversation id.
func listByID(rows []ConversationListing) map[string]ConversationListing {
	out := make(map[string]ConversationListing, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// TestListConversationsTopLevelAndChildren walks the tree by level: the
// top-level call returns the root only, and each expansion returns the next
// level with the ChildCount the next expansion would return.
func TestListConversationsTopLevelAndChildren(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "tree")
	root := listConversation(t, pool, listConvSeed{ownerID: owner, name: "root"})
	kid := listConversation(t, pool, listConvSeed{ownerID: owner, name: "kid", parentID: root})
	thread := listConversation(t, pool, listConvSeed{ownerID: owner, name: "thread", parentID: kid})
	ins := New(pool)

	top, next, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "top-level list")
	c.Eq("", next, "next on a single-row page")
	c.Require().Len(top, 1, "top-level rows = %d, want the root only", len(top))
	c.Eq(root, top[0].ID, "top-level row")
	c.Eq(1, top[0].ChildCount, "root ChildCount")
	c.Eq("", top[0].ParentConversationID, "root ParentConversationID")
	c.Eq("root", top[0].Name, "root Name")

	kids, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{ParentConversationID: root, Excluded: listExcluded})
	c.Require().NoError(err, "expand root")
	c.Require().Len(kids, 1, "rows under root = %d, want the child conversation only", len(kids))
	c.Eq(kid, kids[0].ID, "row under root")
	c.Eq(1, kids[0].ChildCount, "kid ChildCount")
	c.Eq(root, kids[0].ParentConversationID, "kid ParentConversationID")

	threads, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{ParentConversationID: kid, Excluded: listExcluded})
	c.Require().NoError(err, "expand kid")
	c.Require().Len(threads, 1, "rows under kid = %d, want the thread conversation only", len(threads))
	c.Eq(thread, threads[0].ID, "row under kid")
	c.Eq(0, threads[0].ChildCount, "thread ChildCount")
	c.Eq(kid, threads[0].ParentConversationID, "thread ParentConversationID")
}

// TestListConversationsExcludesEntrypointsAndLedgers pins the eligible set:
// recorder entrypoints never list, and a conversation with no nodes and no
// fork is invisible while a fork with no nodes of its own is not.
func TestListConversationsExcludesEntrypointsAndLedgers(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "ledger")
	withMsg := listConversation(t, pool, listConvSeed{ownerID: owner, name: "ledgered", entry: "recall-summary"})
	insertMessage(t, pool, withMsg, 0, "user", `[{"type":"text","text":"ledger body"}]`)
	zero := 0
	barren := listConversation(t, pool, listConvSeed{ownerID: owner, name: "barren", nodeCnt: &zero})
	// The fork source stays nodeless itself, so it is invisible too and the
	// forked row is the only listing.
	forkSrc := listConversation(t, pool, listConvSeed{ownerID: owner, name: "fork source", nodeCnt: &zero})
	forked := listConversation(t, pool, listConvSeed{
		ownerID: owner, name: "forked", nodeCnt: &zero,
		forkFrom: listForkSource(t, pool, forkSrc),
	})
	ins := New(pool)

	rows, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "list")
	c.Require().Len(rows, 1, "listed rows = %d, want the forked conversation only", len(rows))
	c.Eq(forked, rows[0].ID, "the sole listed row")
	byID := listByID(rows)
	c.Require().HasKey(byID, forked, "a nodeless fork must list")
	c.NotHasKey(byID, withMsg, "a recall-summary conversation with messages must not list")
	c.NotHasKey(byID, barren, "a nodeless conversation without a fork must not list")
	c.NotHasKey(byID, forkSrc, "the nodeless fork source must not list")
}

// TestListConversationsStates seeds one conversation per state and pins both
// the derivation (closed over exited over open, no child at all for no_agent)
// and the States filter. The closed one links its child the claude way, by
// external_ref with a NULL conversation_id, exercising the second join arm.
func TestListConversationsStates(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "states")
	past := time.Now().Add(-time.Hour)

	openConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "open-one"})
	exitedConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "exited-one"})
	closedRef := fmt.Sprintf("c-closed-%d", time.Now().UnixNano())
	closedConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "closed-one", ref: closedRef})
	noAgentConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "no-agent-one"})

	listChild(t, pool, listChildSeed{childID: "c-states-open", convID: openConv, kind: "fundi", name: "open child"})
	listChild(t, pool, listChildSeed{childID: "c-states-exited", convID: exitedConv, kind: "fundi", name: "exited child", exitedAt: &past})
	listChild(t, pool, listChildSeed{childID: closedRef, kind: "claude", name: "closed child", closedAt: &past})
	ins := New(pool)

	all, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "list all states")
	byID := listByID(all)
	c.Require().Eq(4, len(byID), "listed rows = %d, want the four fixtures", len(byID))
	c.Eq(ListStateOpen, byID[openConv].State, "open state")
	c.Eq(ListStateExited, byID[exitedConv].State, "exited state")
	c.Eq(ListStateClosed, byID[closedConv].State, "closed state")
	c.Eq(ListStateNoAgent, byID[noAgentConv].State, "no-agent state")
	c.Eq("c-states-open", byID[openConv].ChildID, "child linked by conversation_id")
	c.Eq("fundi", byID[openConv].Kind, "kind of the conversation_id-linked child")
	c.Eq(closedRef, byID[closedConv].ChildID, "child linked by external_ref")

	asked, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		States: []ListState{ListStateOpen, ListStateClosed}, Excluded: listExcluded,
	})
	c.Require().NoError(err, "list by states")
	got := listIDs(asked)
	c.ElementsMatch([]string{openConv, closedConv}, got, "states filter returned")
}

// TestListConversationsMatchedDescendantKeepsAncestor pins the visible set: a
// filter matching only the grandchild still lists the root and the middle
// level, each with the ChildCount its expansion returns.
func TestListConversationsMatchedDescendantKeepsAncestor(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "ancestor")
	needle := fmt.Sprintf("needle-%d", time.Now().UnixNano())
	root := listConversation(t, pool, listConvSeed{ownerID: owner, name: "root-plain"})
	kid := listConversation(t, pool, listConvSeed{ownerID: owner, name: "kid-plain", parentID: root})
	thread := listConversation(t, pool, listConvSeed{ownerID: owner, name: needle, parentID: kid})
	ins := New(pool)

	top, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, Excluded: listExcluded})
	c.Require().NoError(err, "top-level filtered list")
	c.Require().Len(top, 1, "filtered top-level rows = %d, want the root as ancestor", len(top))
	c.Eq(root, top[0].ID, "filtered top-level row")
	c.Eq(1, top[0].ChildCount, "root ChildCount under the filter")

	kids, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, ParentConversationID: root, Excluded: listExcluded})
	c.Require().NoError(err, "expand root under the filter")
	c.Require().Len(kids, 1, "filtered rows under root = %d, want the middle conversation", len(kids))
	c.Eq(kid, kids[0].ID, "filtered row under root")
	c.Eq(1, kids[0].ChildCount, "kid ChildCount under the filter")

	threads, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, ParentConversationID: kid, Excluded: listExcluded})
	c.Require().NoError(err, "expand kid under the filter")
	c.Require().Len(threads, 1, "filtered rows under kid = %d, want the match itself", len(threads))
	c.Eq(thread, threads[0].ID, "filtered row under kid")
	c.Eq(0, threads[0].ChildCount, "thread ChildCount under the filter")

	// Without the filter the same levels list every conversation.
	plain, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{ParentConversationID: root, Excluded: listExcluded})
	c.Require().NoError(err, "unfiltered expand root")
	c.Require().Len(plain, 1, "unfiltered rows under root = %d, want the middle conversation only (it is the only child of the root)", len(plain))
	c.Eq(kid, plain[0].ID, "unfiltered row under root")
}

// listPagingWalk pages through a listing with limit until next is empty,
// returning every row id in walk order.
func listPagingWalk(c *assert.C, ctx context.Context, ins *Insights, scope Scope, f ListFilter, limit int) []string {
	var got []string
	cursor := f.Cursor
	for i := 0; i < 10; i++ {
		f.Cursor = cursor
		f.Limit = limit
		rows, next, err := ins.ListConversations(ctx, scope, f)
		c.Require().NoError(err, "paging walk page %d", i)
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if next == "" {
			return got
		}
		cursor = next
	}
	c.Fail("paging walk did not terminate within 10 pages")
	return got
}

// TestListConversationsPagingRecent walks the recent sort to exhaustion: nine
// rows (seven distinct timestamps, two sharing one) come back exactly once, in
// the declared order, and the last page reports an empty next.
func TestListConversationsPagingRecent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "paging-recent")
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)

	var seeded []string
	for i := 0; i < 7; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seeded = append(seeded, listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: ts, updatedAt: ts}))
	}
	shared := base.Add(10 * time.Minute)
	seeded = append(seeded,
		listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: shared, updatedAt: shared}),
		listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: shared, updatedAt: shared}),
	)
	ins := New(pool)

	var expected []string
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM conversations.conversation WHERE id = ANY($1::uuid[])
		 ORDER BY updated_at DESC, id DESC`, seeded)
	c.Require().NoError(err, "order oracle")
	for rows.Next() {
		var id string
		c.Require().NoError(rows.Scan(&id), "scan oracle row")
		expected = append(expected, id)
	}
	c.Require().NoError(rows.Err(), "oracle rows")
	c.Require().Eq(9, len(expected), "oracle rows = %d, want the nine fixtures", len(expected))

	got := listPagingWalk(c, ctx, ins, ScopeOwner(owner), ListFilter{Excluded: listExcluded, Sort: ListSortRecent}, 3)
	c.EqDeep(expected, got, "walked order")
}

// TestListConversationsPagingCreated walks the created sort the same way.
func TestListConversationsPagingCreated(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "paging-created")
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)

	var seeded []string
	for i := 0; i < 7; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seeded = append(seeded, listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: ts, updatedAt: ts}))
	}
	shared := base.Add(10 * time.Minute)
	seeded = append(seeded,
		listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: shared, updatedAt: shared}),
		listConversation(t, pool, listConvSeed{ownerID: owner, createdAt: shared, updatedAt: shared}),
	)
	ins := New(pool)

	var expected []string
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM conversations.conversation WHERE id = ANY($1::uuid[])
		 ORDER BY created_at DESC, id DESC`, seeded)
	c.Require().NoError(err, "order oracle")
	for rows.Next() {
		var id string
		c.Require().NoError(rows.Scan(&id), "scan oracle row")
		expected = append(expected, id)
	}
	c.Require().NoError(rows.Err(), "oracle rows")
	c.Require().Eq(9, len(expected), "oracle rows = %d, want the nine fixtures", len(expected))

	got := listPagingWalk(c, ctx, ins, ScopeOwner(owner), ListFilter{Excluded: listExcluded, Sort: ListSortCreated}, 3)
	c.EqDeep(expected, got, "walked order")
}

// TestListConversationsQuery pins the BM25 filter and the relevance order: a
// per-run token in the windows of two conversations selects exactly those two,
// relevance puts the conversation whose window holds the token twice first,
// and a database without the index refuses with ErrContentSearchUnavailable.
func TestListConversationsQuery(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	if !listBM25Ready(t, pool) {
		t.Skip("conversation_window_bm25 missing on this database")
	}
	owner := listOwnerUser(t, pool, "query")
	token := fmt.Sprintf("zqxwv%d", time.Now().UnixNano())
	twice := listConversation(t, pool, listConvSeed{ownerID: owner, name: "token twice"})
	once := listConversation(t, pool, listConvSeed{ownerID: owner, name: "token once"})
	other := listConversation(t, pool, listConvSeed{ownerID: owner, name: "no token"})
	// Equal window lengths, differing token count: BM25 orders the denser
	// window strictly first.
	listWindow(t, pool, twice, owner, "aa "+token+" "+token+" bb", 0)
	listWindow(t, pool, once, owner, "aa "+token+" bb cc", 0)
	listWindow(t, pool, other, owner, "nothing relevant here at all", 0)
	ins := New(pool)

	rows, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Query: token, Excluded: listExcluded})
	c.Require().NoError(err, "query listing")
	c.ElementsMatch([]string{twice, once}, listIDs(rows), "query matches")
	for _, r := range rows {
		c.NotEq(other, r.ID, "the conversation without the token listed")
	}

	rel, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		Query: token, Sort: ListSortRelevance, Excluded: listExcluded,
	})
	c.Require().NoError(err, "relevance listing")
	c.Require().Len(rel, 2, "relevance rows = %d, want both matches", len(rel))
	c.Eq(twice, rel[0].ID, "the window holding the token twice sorts first")
	c.Eq(once, rel[1].ID, "the window holding the token once sorts second")

	// A relevance cursor pages by score: walk both rows through the cursor.
	relWalk := listPagingWalk(c, ctx, ins, ScopeOwner(owner), ListFilter{
		Query: token, Sort: ListSortRelevance, Excluded: listExcluded,
	}, 1)
	c.EqDeep([]string{twice, once}, relWalk, "relevance walk")

	// Without the index the query refuses instead of returning wrong rows.
	_, err = pool.Exec(ctx, "DROP INDEX conversations.conversation_window_bm25")
	c.Require().NoError(err, "drop the bm25 index")
	_, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Query: token, Excluded: listExcluded})
	c.ErrorIs(err, ErrContentSearchUnavailable, "query without the bm25 index")
}

// TestListConversationsRelevanceNeedsQuery pins that the relevance sort
// without a query is refused before any rows are read.
func TestListConversationsRelevanceNeedsQuery(t *testing.T) {
	c := assert.NewAborting(t)
	pool := newTestPool(t)
	ins := New(pool)

	_, _, err := ins.ListConversations(context.Background(), ScopeAll(), ListFilter{Sort: ListSortRelevance})
	c.ErrorIs(err, ErrRelevanceNeedsQuery, "relevance sort without a query")
}

// TestListConversationsTurnsAndCost pins the page aggregates: Turns counts the
// conversation's turns and CostUSD prices them through model_pricing, the same
// two laterals Search uses.
func TestListConversationsTurnsAndCost(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "cost")
	conv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "priced"})
	listPricing(t, pool, "priced-model", 0.001, 0.002, 0.0005)
	insertTurn(t, pool, conv, seedTurn{ordinal: 0, model: "priced-model", source: "claude", inTok: 100, outTok: 50, cacheRead: 10})
	insertTurn(t, pool, conv, seedTurn{ordinal: 1, model: "priced-model", source: "claude", inTok: 100, outTok: 50, cacheRead: 10})
	ins := New(pool)

	rows, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "list")
	c.Require().Len(rows, 1, "rows = %d, want the priced conversation", len(rows))
	c.Eq(2, rows[0].Turns, "Turns")
	// Each turn costs 100*0.001 + 50*0.002 + 10*0.0005 = 0.205.
	want := 2 * 0.205
	c.True(math.Abs(rows[0].CostUSD-want) < 1e-9, "CostUSD = %v, want %v", rows[0].CostUSD, want)
}

// TestVisible pins the membership probe: in-scope true, another owner's row
// false, an unknown uuid false, and a malformed id false with a nil error.
func TestVisible(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "visible")
	other := listOwnerUser(t, pool, "visible-other")
	conv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "mine"})
	ins := New(pool)

	ok, err := ins.Visible(ctx, ScopeOwner(owner), conv)
	c.Require().NoError(err, "visible in scope")
	c.True(ok, "an in-scope conversation must be visible")

	ok, err = ins.Visible(ctx, ScopeOwner(other), conv)
	c.Require().NoError(err, "visible out of scope")
	c.False(ok, "another owner's conversation must not be visible")

	ok, err = ins.Visible(ctx, ScopeOwner(owner), "0b1e2f3a-1111-4222-8333-444455556666")
	c.Require().NoError(err, "visible unknown uuid")
	c.False(ok, "an unknown uuid must not be visible")

	ok, err = ins.Visible(ctx, ScopeOwner(owner), "not-a-uuid")
	c.NoError(err, "visible malformed id")
	c.False(ok, "a malformed id must not be visible")
}
