// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
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
	id        string    // explicit id, for tests that need a controlled uuid; uuidv7() when empty
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
	var idArg any
	if s.id != "" {
		idArg = s.id
	}
	var id string
	err := pool.QueryRow(context.Background(), `
INSERT INTO conversations.conversation
    (owner_user_id, name, model, origin_entrypoint, driven_by, external_ref, cwd,
     node_count, fork_from, parent_conversation_id, created_at, updated_at, closed_at, id)
VALUES ($1::uuid, NULLIF($2,''), NULLIF($3,''), $4, 'client', NULLIF($5,''), NULLIF($6,''),
        $7, $8::uuid, $9::uuid, $10, $11, $12, coalesce($13::uuid, uuidv7()))
RETURNING id::text`,
		ownerArg, s.name, s.model, entry, s.ref, s.cwd, nodeCnt, forkArg, parentArg,
		created, updated, s.closedAt, idArg).Scan(&id)
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

	// Spellings uuid.Parse tolerates but Postgres' ::uuid cast refuses must
	// answer exactly like any other malformed id: (false, nil), never a SQL
	// error.
	for _, spelling := range []string{
		"urn:uuid:" + conv,
		"{" + conv + "}",
		strings.ReplaceAll(conv, "-", ""),
		strings.ToUpper(conv),
		"urn:uuid:0b1e2f3a-1111-4222-8333-444455556666",
	} {
		ok, err = ins.Visible(ctx, ScopeOwner(owner), spelling)
		c.NoError(err, "visible non-canonical spelling %q", spelling)
		c.False(ok, "non-canonical spelling %q must not be visible", spelling)
	}
}

// TestListConversationsSharedConversationChildPickedOnce pins the fan-out
// guard: child_conversation_idx is not unique, so two child rows can share one
// conversation. The conversation lists once, its parent's ChildCount counts it
// once, and the linked child is the deterministic pick, the lowest child_id.
func TestListConversationsSharedConversationChildPickedOnce(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "fanout")
	root := listConversation(t, pool, listConvSeed{ownerID: owner, name: "root"})
	shared := listConversation(t, pool, listConvSeed{ownerID: owner, name: "shared", parentID: root})
	listChild(t, pool, listChildSeed{childID: "c-bbb-shared", convID: shared, kind: "fundi", name: "second"})
	listChild(t, pool, listChildSeed{childID: "c-aaa-shared", convID: shared, kind: "fundi", name: "first"})
	ins := New(pool)

	top, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "top-level list")
	c.Require().Len(top, 1, "top-level rows = %d, want the root once", len(top))
	c.Eq(1, top[0].ChildCount, "root ChildCount with a twice-linked child")

	kids, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{ParentConversationID: root, Excluded: listExcluded})
	c.Require().NoError(err, "expand root")
	c.Require().Len(kids, 1, "rows under root = %d, want the shared conversation once", len(kids))
	c.Eq(shared, kids[0].ID, "row under root")
	c.Eq("c-aaa-shared", kids[0].ChildID, "the lowest child_id wins the link")
	c.Eq("fundi", kids[0].Kind, "kind of the picked child")
}

// TestListConversationsIlikeEscapesWildcards pins that NameContains matches
// the literal text: %, _ and \ in the filter are escaped, so foo_bar does not
// match fooxbar.
func TestListConversationsIlikeEscapesWildcards(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	tok := time.Now().UnixNano()
	owner := listOwnerUser(t, pool, "ilike")
	underscore := listConversation(t, pool, listConvSeed{ownerID: owner, name: fmt.Sprintf("foo_bar-%d", tok)})
	decoy := listConversation(t, pool, listConvSeed{ownerID: owner, name: fmt.Sprintf("fooxbar-%d", tok)})
	pct := listConversation(t, pool, listConvSeed{ownerID: owner, name: fmt.Sprintf("50%%off-%d", tok)})
	pctDecoy := listConversation(t, pool, listConvSeed{ownerID: owner, name: fmt.Sprintf("50Xoff-%d", tok)})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: "foo_bar", Excluded: listExcluded})
	c.Require().NoError(err, "name filter with an underscore")
	c.EqDeep([]string{underscore}, listIDs(got), "foo_bar must match only the literal name")
	c.NotContains(listIDs(got), decoy, "the underscore acted as a wildcard")

	got, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: "50%off", Excluded: listExcluded})
	c.Require().NoError(err, "name filter with a percent")
	c.EqDeep([]string{pct}, listIDs(got), "50%off must match only the literal name")
	c.NotContains(listIDs(got), pctDecoy, "the percent acted as a wildcard")
}

// TestListConversationsOuterOrderBreaksTiesByUUID pins the outer ORDER BY's
// tie-break: rows tied on the sort key finish in uuid order, the same ordering
// the page and the keyset use, so the row the cursor is minted from is the
// page's true last row. The two ids tie on updated_at and differ only in their
// uuids. On this cluster's builtin C.UTF-8 provider a canonical lowercase uuid
// renders as text in the same order as its bytes, so the pre-fix unqualified
// id (resolving to the p.id::text output column) is outcome-equivalent here;
// the test still fails if the outer sort loses or flips the uuid tie-break.
func TestListConversationsOuterOrderBreaksTiesByUUID(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "tiebreak")
	idLow := "0a000000-0000-7000-8000-000000000000"
	idHigh := "0b000000-0000-7000-8000-000000000000"
	ts := time.Now().Add(-time.Hour).Truncate(time.Second)
	listConversation(t, pool, listConvSeed{ownerID: owner, id: idLow, name: "tie-a", updatedAt: ts})
	listConversation(t, pool, listConvSeed{ownerID: owner, id: idHigh, name: "tie-b", updatedAt: ts})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "tied listing")
	c.EqDeep([]string{idHigh, idLow}, listIDs(got), "ties must break by uuid descending")
}

// TestListConversationsKindsFilter pins the kind filter and that a non-empty
// list excludes no-agent rows.
func TestListConversationsKindsFilter(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "kinds")
	fundiConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "fundi-one"})
	claudeConv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "claude-one"})
	listConversation(t, pool, listConvSeed{ownerID: owner, name: "no-agent"})
	listChild(t, pool, listChildSeed{childID: "c-kinds-fundi", convID: fundiConv, kind: "fundi"})
	listChild(t, pool, listChildSeed{childID: "c-kinds-claude", convID: claudeConv, kind: "claude"})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Kinds: []string{"fundi"}, Excluded: listExcluded})
	c.Require().NoError(err, "kinds filter fundi")
	c.EqDeep([]string{fundiConv}, listIDs(got), "kinds fundi")

	got, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Kinds: []string{"fundi", "claude"}, Excluded: listExcluded})
	c.Require().NoError(err, "kinds filter both")
	c.ElementsMatch([]string{fundiConv, claudeConv}, listIDs(got), "kinds fundi+claude must list both children and exclude the no-agent row")
}

// TestListConversationsLabelsFilter pins the jsonb containment against the
// linked child's labels.
func TestListConversationsLabelsFilter(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "labels")
	matched := listConversation(t, pool, listConvSeed{ownerID: owner, name: "labelled"})
	other := listConversation(t, pool, listConvSeed{ownerID: owner, name: "other labels"})
	bare := listConversation(t, pool, listConvSeed{ownerID: owner, name: "no labels"})
	listChild(t, pool, listChildSeed{childID: "c-labels-a", convID: matched, kind: "fundi", labels: map[string]string{"rafiki/parent": "p-1"}})
	listChild(t, pool, listChildSeed{childID: "c-labels-b", convID: other, kind: "fundi", labels: map[string]string{"other": "x"}})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		Labels: map[string]string{"rafiki/parent": "p-1"}, Excluded: listExcluded,
	})
	c.Require().NoError(err, "labels filter")
	c.EqDeep([]string{matched}, listIDs(got), "labels containment must match only the child holding the pair")
	c.NotContains(listIDs(got), other, "a child without the pair must not match")
	c.NotContains(listIDs(got), bare, "a no-agent row must not match a labels filter")
}

// TestListConversationsCwdContainsArms pins both cwd arms of the filter: the
// conversation's own cwd and the linked child's cwd.
func TestListConversationsCwdContainsArms(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "cwd")
	tok := time.Now().UnixNano()
	convCwd := listConversation(t, pool, listConvSeed{ownerID: owner, name: "conv cwd", cwd: fmt.Sprintf("/repo/alpha-%d/work", tok)})
	childCwd := listConversation(t, pool, listConvSeed{ownerID: owner, name: "child cwd"})
	listChild(t, pool, listChildSeed{childID: "c-cwd-child", convID: childCwd, kind: "fundi", cwd: fmt.Sprintf("/repo/beta-%d/work", tok)})
	other := listConversation(t, pool, listConvSeed{ownerID: owner, name: "elsewhere", cwd: fmt.Sprintf("/elsewhere-%d", tok)})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{CwdContains: fmt.Sprintf("/repo/alpha-%d", tok), Excluded: listExcluded})
	c.Require().NoError(err, "cwd filter, conversation arm")
	c.EqDeep([]string{convCwd}, listIDs(got), "the conversation cwd arm")

	got, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{CwdContains: fmt.Sprintf("/repo/beta-%d", tok), Excluded: listExcluded})
	c.Require().NoError(err, "cwd filter, child arm")
	c.EqDeep([]string{childCwd}, listIDs(got), "the child cwd arm")
	c.NotContains(listIDs(got), other, "an unrelated cwd must not match")
}

// TestListConversationsSinceUntilBounds pins the updated_at window: Since is
// inclusive, Until exclusive, and each bound works alone.
func TestListConversationsSinceUntilBounds(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "window")
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	first := listConversation(t, pool, listConvSeed{ownerID: owner, updatedAt: base})
	middle := listConversation(t, pool, listConvSeed{ownerID: owner, updatedAt: base.Add(time.Hour)})
	last := listConversation(t, pool, listConvSeed{ownerID: owner, updatedAt: base.Add(2 * time.Hour)})
	ins := New(pool)

	since := base.Add(30 * time.Minute)
	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Since: &since, Excluded: listExcluded})
	c.Require().NoError(err, "since only")
	c.ElementsMatch([]string{middle, last}, listIDs(got), "since must keep rows at and after the bound")

	until := base.Add(90 * time.Minute)
	got, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Until: &until, Excluded: listExcluded})
	c.Require().NoError(err, "until only")
	c.ElementsMatch([]string{first, middle}, listIDs(got), "until must exclude the bound itself")

	got, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Since: &since, Until: &until, Excluded: listExcluded})
	c.Require().NoError(err, "since and until")
	c.EqDeep([]string{middle}, listIDs(got), "the combined window keeps only the middle row")
}

// TestListConversationsNameMatchesChildName pins the child-name arm of
// NameContains: a conversation whose own name misses but whose linked child's
// name hits is listed.
func TestListConversationsNameMatchesChildName(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "childname")
	needle := fmt.Sprintf("childname-%d", time.Now().UnixNano())
	conv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "plain conversation"})
	listChild(t, pool, listChildSeed{childID: "c-childname", convID: conv, kind: "fundi", name: needle})
	other := listConversation(t, pool, listConvSeed{ownerID: owner, name: "another conversation"})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, Excluded: listExcluded})
	c.Require().NoError(err, "name filter over the child arm")
	c.EqDeep([]string{conv}, listIDs(got), "the child's name must match for its conversation")
	c.NotContains(listIDs(got), other, "a conversation without the name must not match")
}

// TestListConversationsScopeExcludesOtherOwners puts two owners in the SAME
// scratch database and pins that the scope predicate keeps the listing to the
// scoped owner's rows.
func TestListConversationsScopeExcludesOtherOwners(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	ownerA := listOwnerUser(t, pool, "scope-a")
	ownerB := listOwnerUser(t, pool, "scope-b")
	convA := listConversation(t, pool, listConvSeed{ownerID: ownerA, name: "a's"})
	convB := listConversation(t, pool, listConvSeed{ownerID: ownerB, name: "b's"})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(ownerA), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "scoped listing")
	c.EqDeep([]string{convA}, listIDs(got), "scope owner a")
	c.NotContains(listIDs(got), convB, "another owner's conversation in the same database must never list")

	got, _, err = ins.ListConversations(ctx, ScopeOwner(ownerB), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "scoped listing, other owner")
	c.EqDeep([]string{convB}, listIDs(got), "scope owner b")
}

// TestListConversationsClosedOutranksExited pins the state precedence: a child
// carrying both timestamps is closed, never exited.
func TestListConversationsClosedOutranksExited(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "precedence")
	past := time.Now().Add(-time.Hour)
	conv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "both stamps"})
	listChild(t, pool, listChildSeed{childID: "c-both-stamps", convID: conv, kind: "fundi", exitedAt: &past, closedAt: &past})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "list")
	c.Require().Len(got, 1, "rows = %d, want the stamped conversation", len(got))
	c.Eq(ListStateClosed, got[0].State, "closed_at must outrank exited_at")
}

// TestListConversationsConversationArmWinsOverRefArm pins COALESCE(k1, k2):
// when both join arms match, the conversation_id-linked child is the one shown.
func TestListConversationsConversationArmWinsOverRefArm(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "arms")
	conv := listConversation(t, pool, listConvSeed{ownerID: owner, name: "both arms", ref: "c-ref-child"})
	listChild(t, pool, listChildSeed{childID: "c-conv-child", convID: conv, kind: "fundi", name: "by conversation"})
	listChild(t, pool, listChildSeed{childID: "c-ref-child", kind: "claude", name: "by external_ref"})
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "list")
	c.Require().Len(got, 1, "rows = %d, want the conversation once (both arms match)", len(got))
	c.Eq("c-conv-child", got[0].ChildID, "the conversation_id arm must win")
	c.Eq("by conversation", got[0].ChildName, "the winning child's columns must ride along")
}

// TestListConversationsLimitDefault pins Limit <= 0 listing 50 rows.
func TestListConversationsLimitDefault(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "default-limit")
	_, err := pool.Exec(ctx, `
INSERT INTO conversations.conversation (owner_user_id, name, node_count, origin_entrypoint, driven_by)
SELECT $1::uuid, 'row-' || g, 1, 'test', 'client' FROM generate_series(1, 51) g`, owner)
	c.Require().NoError(err, "seed 51 rows")
	ins := New(pool)

	rows, next, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "default listing")
	c.Len(rows, 50, "the default limit must be 50")
	c.NotEq("", next, "one row must remain for the next page")
}

// TestListConversationsLimitClampsToMax pins that a Limit above 500 is clamped
// to 500.
func TestListConversationsLimitClampsToMax(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "clamp-limit")
	_, err := pool.Exec(ctx, `
INSERT INTO conversations.conversation (owner_user_id, name, node_count, origin_entrypoint, driven_by)
SELECT $1::uuid, 'row-' || g, 1, 'test', 'client' FROM generate_series(1, 501) g`, owner)
	c.Require().NoError(err, "seed 501 rows")
	ins := New(pool)

	rows, next, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Limit: 1000, Excluded: listExcluded})
	c.Require().NoError(err, "clamped listing")
	c.Len(rows, 500, "a Limit above 500 must clamp to 500")
	c.NotEq("", next, "one row must remain for the next page")
}

// TestListConversationsParentFilterBadIDIsEmptyPage pins the parent filter's
// refusal shape: a malformed spelling, a non-canonical spelling and an unknown
// uuid all return an empty page with no error.
func TestListConversationsParentFilterBadIDIsEmptyPage(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "badparent")
	listConversation(t, pool, listConvSeed{ownerID: owner, name: "top"})
	ins := New(pool)

	for _, bad := range []string{
		"not-a-uuid",
		"urn:uuid:0b1e2f3a-1111-4222-8333-444455556666",
		"{0b1e2f3a-1111-4222-8333-444455556666}",
		"0b1e2f3a111142228333444455556666",
		"0b1e2f3a-1111-4222-8333-444455556666",
	} {
		rows, next, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{ParentConversationID: bad, Excluded: listExcluded})
		c.NoError(err, "parent %q", bad)
		c.Empty(rows, "parent %q must return no rows", bad)
		c.Eq("", next, "parent %q must return no cursor", bad)
	}
}

// TestListConversationsCursorMismatchesRefused pins ErrBadCursor through the
// listing call itself: a cursor minted for one sort or parent cannot page
// another request's shape.
func TestListConversationsCursorMismatchesRefused(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "cursorguard")
	ts := time.Now().Add(-time.Hour)
	listConversation(t, pool, listConvSeed{ownerID: owner, updatedAt: ts})
	ins := New(pool)

	at := ts
	raw := encodeListCursor(ListSortRecent, "", "0b1e2f3a-1111-4222-8333-444455556666", &at, nil)

	_, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		Sort: ListSortCreated, Cursor: raw, Excluded: listExcluded,
	})
	c.ErrorIs(err, ErrBadCursor, "a recent cursor cannot page a created request")

	_, _, err = ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		ParentConversationID: owner, Cursor: raw, Excluded: listExcluded,
	})
	c.ErrorIs(err, ErrBadCursor, "a top-level cursor cannot page under a parent")
}

// TestListConversationsPagingUnderParent pins keyset paging below a non-empty
// parent and that the parent's ChildCount equals what the expansion returns.
func TestListConversationsPagingUnderParent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "pageparent")
	root := listConversation(t, pool, listConvSeed{ownerID: owner, name: "root"})
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		listConversation(t, pool, listConvSeed{
			ownerID: owner, name: fmt.Sprintf("kid-%d", i), parentID: root, updatedAt: ts,
		})
	}
	ins := New(pool)

	var expected []string
	oracle, err := pool.Query(ctx, `
SELECT id::text FROM conversations.conversation
WHERE parent_conversation_id = $1::uuid
ORDER BY updated_at DESC, id DESC`, root)
	c.Require().NoError(err, "order oracle")
	for oracle.Next() {
		var id string
		c.Require().NoError(oracle.Scan(&id), "scan oracle row")
		expected = append(expected, id)
	}
	c.Require().NoError(oracle.Err(), "oracle rows")
	c.Require().Eq(5, len(expected), "oracle rows = %d, want the five children", len(expected))

	got := listPagingWalk(c, ctx, ins, ScopeOwner(owner), ListFilter{ParentConversationID: root, Excluded: listExcluded}, 2)
	c.EqDeep(expected, got, "paging under a parent must walk every child exactly once, in order")

	top, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{Excluded: listExcluded})
	c.Require().NoError(err, "top-level listing for the count")
	c.Require().Len(top, 1, "top-level rows = %d, want the root", len(top))
	c.Eq(len(expected), top[0].ChildCount, "ChildCount must equal what the expansion returns")
}

// TestListConversationsRelevanceAncestorScore pins ancestor-score propagation:
// a top-level conversation with no window of its own sorts by its best
// descendant's score, ahead of a matched conversation with a weaker hit.
func TestListConversationsRelevanceAncestorScore(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	if !listBM25Ready(t, pool) {
		t.Skip("conversation_window_bm25 missing on this database")
	}
	owner := listOwnerUser(t, pool, "ancestor-score")
	token := fmt.Sprintf("zqxwv%d", time.Now().UnixNano())
	// The ids force the defensive case: with M's uuid below R's, a tie would
	// order M first under the relevance sort's id-ASC tiebreak, so the test
	// fails unless R takes T1's strictly better score.
	root := "0f000000-0000-7000-8000-000000000000"
	rival := "0e000000-0000-7000-8000-000000000000"
	listConversation(t, pool, listConvSeed{ownerID: owner, id: root, name: "ancestor only"})
	rivalConv := listConversation(t, pool, listConvSeed{ownerID: owner, id: rival, name: "rival"})
	best := listConversation(t, pool, listConvSeed{ownerID: owner, name: "best hit", parentID: root})
	worse := listConversation(t, pool, listConvSeed{ownerID: owner, name: "weaker hit", parentID: root})
	listWindow(t, pool, best, owner, "aa "+token+" "+token+" bb", 0)
	listWindow(t, pool, worse, owner, "aa "+token+" bb cc", 0)
	listWindow(t, pool, rivalConv, owner, "aa "+token+" bb cc", 0)
	ins := New(pool)

	got, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		Query: token, Sort: ListSortRelevance, Excluded: listExcluded,
	})
	c.Require().NoError(err, "top-level relevance listing")
	c.EqDeep([]string{root, rival}, listIDs(got), "the ancestor must sort with its best descendant's score, ahead of the weaker rival")

	kids, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{
		Query: token, Sort: ListSortRelevance, ParentConversationID: root, Excluded: listExcluded,
	})
	c.Require().NoError(err, "expansion under the ancestor")
	c.EqDeep([]string{best, worse}, listIDs(kids), "the descendants order by their own scores")
}

// TestListConversationsChildCountUnderFilter pins ChildCount against the
// filtered visible set: children the filter drops must not be counted, and the
// count must equal what an expansion with the same filter returns.
func TestListConversationsChildCountUnderFilter(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := listOwnerUser(t, pool, "filteredcount")
	needle := fmt.Sprintf("countme-%d", time.Now().UnixNano())
	root := listConversation(t, pool, listConvSeed{ownerID: owner, name: "root"})
	matching := listConversation(t, pool, listConvSeed{ownerID: owner, name: needle, parentID: root})
	hidden := listConversation(t, pool, listConvSeed{ownerID: owner, name: "unmatched child", parentID: root})
	ins := New(pool)

	top, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, Excluded: listExcluded})
	c.Require().NoError(err, "filtered top-level listing")
	c.Require().Len(top, 1, "filtered top-level rows = %d, want the root", len(top))
	c.Eq(1, top[0].ChildCount, "ChildCount must exclude the child the filter drops")

	kids, _, err := ins.ListConversations(ctx, ScopeOwner(owner), ListFilter{NameContains: needle, ParentConversationID: root, Excluded: listExcluded})
	c.Require().NoError(err, "filtered expansion")
	c.Require().Len(kids, 1, "filtered rows under root = %d, want the match only", len(kids))
	c.Eq(matching, kids[0].ID, "the matched child")
	c.Eq(top[0].ChildCount, len(kids), "ChildCount must equal the expansion's row count")
	c.NotContains(listIDs(kids), hidden, "the filtered-out child must not be returned")
}
