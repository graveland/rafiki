// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// Every id this file mints is unique per run (the shared test database is
// never reset) and contains '_', which a LIKE-based reader would read as a
// wildcard -- these paths match by equality, and the ids keep proving that.

// insertStampChild writes one conversations.child row directly, the way a
// spawn's first writeRecord would: labels carrying the parent link,
// conversation_id NULL for a claude-shaped child or set for a fundi one.
func insertStampChild(t *testing.T, pool *pgxpool.Pool, childID, parentID string, conversationID *string, legacyParent bool) {
	t.Helper()
	labels := fmt.Sprintf(`{"rafiki/parent":%q}`, parentID)
	if legacyParent {
		labels = fmt.Sprintf(`{"fundi/parent":%q}`, parentID)
	}
	var sql string
	var args []any
	if conversationID != nil {
		sql = `INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels, conversation_id)
		       VALUES ($1, 'fundi', 'idle', now(), $2::jsonb, $3::uuid)`
		args = []any{childID, labels, conversationID}
	} else {
		sql = `INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels)
		       VALUES ($1, 'claude', 'idle', now(), $2::jsonb)`
		args = []any{childID, labels}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, sql, args...)
	assert.NewAborting(t).NoError(err, "insert child %s", childID)
}

// relabelStampChild points an existing child row's parent label somewhere
// else, the way a relabel or a re-parent would.
func relabelStampChild(t *testing.T, pool *pgxpool.Pool, childID, parentID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`UPDATE conversations.child SET labels = jsonb_set(labels, '{rafiki/parent}', to_jsonb($2::text))
		  WHERE child_id = $1`, childID, parentID)
	assert.NewAborting(t).NoError(err, "relabel child %s", childID)
}

// insertStampConversation writes one conversation row linked to a child by
// external_ref and returns its id.
func insertStampConversation(t *testing.T, pool *pgxpool.Pool, extRef string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref)
		 VALUES ('test', 'server', $1) RETURNING id::text`, extRef).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation for %s", extRef)
	return id
}

// cleanupStampRows removes this run's child and conversation rows. The test
// database is shared and never reset, so a test must not leave rows behind.
func cleanupStampRows(t *testing.T, pool *pgxpool.Pool, childIDs, convIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx,
			`DELETE FROM conversations.child WHERE child_id = ANY($1)`, childIDs); err != nil {
			t.Logf("cleanup children: %v", err)
		}
		if len(convIDs) > 0 {
			if _, err := pool.Exec(ctx,
				`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`, convIDs); err != nil {
				t.Logf("cleanup conversations: %v", err)
			}
		}
	})
}

// stampParentOf reads a conversation's parent_conversation_id ("" when NULL)
// and its updated_at in one query.
func stampParentOf(t *testing.T, pool *pgxpool.Pool, convID string) (string, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var parent *string
	var updated time.Time
	err := pool.QueryRow(ctx,
		`SELECT parent_conversation_id::text, updated_at FROM conversations.conversation WHERE id = $1::uuid`,
		convID).Scan(&parent, &updated)
	assert.NewAborting(t).NoError(err, "read conversation %s", convID)
	if parent == nil {
		return "", updated
	}
	return *parent, updated
}

// A fundi child carries its conversation id on its own row, and the stamp
// points it at the parent child's conversation.
func TestStampConversationParentFundiChildUsesItsStoredConversation(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_par_parent"
	childID := "c_" + uniq + "_par_child"

	parentConv := insertStampConversation(t, pool, parentID)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv, parentConv})
	insertStampChild(t, pool, parentID, "", &parentConv, false)
	insertStampChild(t, pool, childID, parentID, &childConv, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "stamp")

	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "the child's conversation must name the parent child's conversation")
}

// A claude-shaped child leaves conversation_id NULL and is linked by
// external_ref instead; the stamp resolves both sides the same way.
func TestStampConversationParentClaudeChildResolvesByExternalRef(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_claude_parent"
	childID := "c_" + uniq + "_claude_child"

	parentConv := insertStampConversation(t, pool, parentID)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv, parentConv})
	insertStampChild(t, pool, parentID, "", nil, false)
	insertStampChild(t, pool, childID, parentID, nil, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "stamp")

	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "an external_ref-linked child must be stamped too")
}

// A parent whose conversation does not exist yet leaves the child NULL, and
// the retry after the parent's conversation appears stamps it -- the
// liveness the next row write pays for.
func TestStampConversationParentRetriesUntilTheParentsConversationExists(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_late_parent"
	childID := "c_" + uniq + "_late_child"

	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv})
	insertStampChild(t, pool, parentID, "", nil, false)
	insertStampChild(t, pool, childID, parentID, nil, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "first stamp")
	got, _ := stampParentOf(t, pool, childConv)
	c.Eq("", got, "a parent without a conversation must leave the child unstamped")

	parentConv := insertStampConversation(t, pool, parentID)
	cleanupStampRows(t, pool, nil, []string{parentConv})
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "retry stamp")
	got, _ = stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "the retry must stamp once the parent's conversation exists")
}

// The stamp only fills a NULL: a conversation that already names a parent is
// never re-pointed, even when the child's labels have moved on.
func TestStampConversationParentNeverOverwrites(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	firstParent := "c_" + uniq + "_keep_first"
	secondParent := "c_" + uniq + "_keep_second"
	childID := "c_" + uniq + "_keep_child"

	firstConv := insertStampConversation(t, pool, firstParent)
	secondConv := insertStampConversation(t, pool, secondParent)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, firstParent, secondParent},
		[]string{firstConv, secondConv, childConv})
	insertStampChild(t, pool, firstParent, "", &firstConv, false)
	insertStampChild(t, pool, secondParent, "", &secondConv, false)
	insertStampChild(t, pool, childID, firstParent, &childConv, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "first stamp")

	// The child is re-parented under secondParent, whose conversation exists,
	// so a stamp without the NULL guard WOULD re-point the row.
	relabelStampChild(t, pool, childID, secondParent)
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "second stamp")

	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(firstConv, got, "an existing parent_conversation_id must be kept")
}

// The stamp is bookkeeping about the tree's shape, not activity: updated_at
// is untouched, whatever else the write carries.
func TestStampConversationParentLeavesUpdatedAtAlone(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_touch_parent"
	childID := "c_" + uniq + "_touch_child"

	parentConv := insertStampConversation(t, pool, parentID)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv, parentConv})
	insertStampChild(t, pool, parentID, "", &parentConv, false)
	insertStampChild(t, pool, childID, parentID, &childConv, false)

	_, beforeUpdated := stampParentOf(t, pool, childConv)
	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "stamp")
	got, afterUpdated := stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "the stamp must land")
	c.True(beforeUpdated.Equal(afterUpdated), "updated_at moved: %v -> %v", beforeUpdated, afterUpdated)
}

// A top-level child has no parent to resolve: the update matches nothing and
// the conversation stays top level.
func TestStampConversationParentTopLevelChildIsUntouched(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	childID := "c_" + uniq + "_top_child"

	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID}, []string{childConv})
	insertStampChild(t, pool, childID, "", &childConv, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "stamp")
	got, _ := stampParentOf(t, pool, childConv)
	c.Eq("", got, "a top-level child's conversation must stay top level")
}

// The legacy fundi/parent spelling resolves exactly like rafiki/parent.
func TestStampConversationParentReadsTheLegacyLabel(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_legacy_parent"
	childID := "c_" + uniq + "_legacy_child"

	parentConv := insertStampConversation(t, pool, parentID)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv, parentConv})
	insertStampChild(t, pool, parentID, "", &parentConv, false)
	insertStampChild(t, pool, childID, parentID, &childConv, true)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "stamp")
	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "the legacy fundi/parent label must resolve")
}

// A daemon without a database pool stamps nothing and answers nil.
func TestStampConversationParentWithoutAPoolAnswersNil(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := &Controller{}
	c.NoError(ctrl.stampConversationParent(context.Background(), "c_nope"), "stamp without a pool")
}

// TestStampConversationParentRunsFromWriteRecord pins the call site: the row
// write's success is what arms the stamp, gated on the snapshot's parent
// label, so a spawned child's conversation gains its parent on the write that
// created it -- and a top-level child's write stamps nothing.
func TestStampConversationParentRunsFromWriteRecord(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	parentID := "c_" + uniq + "_write_parent"
	childID := "c_" + uniq + "_write_child"

	parentConv := insertStampConversation(t, pool, parentID)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, parentID}, []string{childConv, parentConv})
	insertStampChild(t, pool, parentID, "", &parentConv, false)

	ctrl := &Controller{
		pool:     pool,
		st:       childstore.New(),
		cm:       newChildManager(),
		children: childstoredb.New(pool),
	}
	ctrl.st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    protocol.StatusIdle,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
		Labels:    map[string]string{childstore.LabelParent: parentID},
	})
	c.NoError(ctrl.writeRecord(childID), "writeRecord")

	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(parentConv, got, "the row write must stamp the parent conversation")

	// A top-level child's write stamps nothing: same fixture shape, no
	// parent label.
	topID := "c_" + uniq + "_write_top"
	topConv := insertStampConversation(t, pool, topID)
	cleanupStampRows(t, pool, []string{topID}, []string{topConv})
	ctrl.st.Insert(&childstore.Session{
		ChildID:   topID,
		Status:    protocol.StatusIdle,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
	})
	c.NoError(ctrl.writeRecord(topID), "writeRecord top level")
	got, _ = stampParentOf(t, pool, topConv)
	c.Eq("", got, "a top-level child's write must not stamp anything")
}

// unsetStampParent clears a conversation's parent link, so a test can watch
// whether a later stamp LOOKS again: the stamp only fills NULLs, so a skip
// and a refusal look identical unless the link is cleared first.
func unsetStampParent(t *testing.T, pool *pgxpool.Pool, convID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET parent_conversation_id = NULL WHERE id = $1::uuid`, convID)
	assert.NewAborting(t).NoError(err, "unset parent of %s", convID)
}

// TestStampConversationParentSecondWriteSkipsTheLookups pins the memo: once a
// stamp has landed for a child, a later row write for that child must not
// query again. The probe clears the conversation's parent link and re-points
// the child's labels at a second parent whose conversation exists, so a stamp
// that still looked would fill the second parent's conversation; a memoized
// skip leaves it NULL.
func TestStampConversationParentSecondWriteSkipsTheLookups(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	firstParent := "c_" + uniq + "_memo_first"
	secondParent := "c_" + uniq + "_memo_second"
	childID := "c_" + uniq + "_memo_child"

	firstConv := insertStampConversation(t, pool, firstParent)
	secondConv := insertStampConversation(t, pool, secondParent)
	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID, firstParent, secondParent},
		[]string{firstConv, secondConv, childConv})
	insertStampChild(t, pool, firstParent, "", &firstConv, false)
	insertStampChild(t, pool, secondParent, "", &secondConv, false)
	insertStampChild(t, pool, childID, firstParent, &childConv, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "first stamp")
	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(firstConv, got, "the first stamp must land")

	unsetStampParent(t, pool, childConv)
	relabelStampChild(t, pool, childID, secondParent)
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "second stamp")
	got, _ = stampParentOf(t, pool, childConv)
	c.Eq("", got, "a memoized child must not be looked at again; got %s", got)
}

// TestStampConversationParentAdoptedConversationIsMemoizedToo pins the
// zero-row leg of the memo: a conversation this daemon did NOT stamp (it
// already carried a parent when the daemon first wrote the row) is
// discovered stamped on that first write and memoized, so it costs nothing
// afterwards either. The probe is the same clear-and-relabel trick as above.
func TestStampConversationParentAdoptedConversationIsMemoizedToo(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	firstParent := "c_" + uniq + "_adopt_first"
	secondParent := "c_" + uniq + "_adopt_second"
	childID := "c_" + uniq + "_adopt_child"

	firstConv := insertStampConversation(t, pool, firstParent)
	secondConv := insertStampConversation(t, pool, secondParent)
	childConv := insertStampConversationWithParent(t, pool, childID, firstConv)
	cleanupStampRows(t, pool, []string{childID, firstParent, secondParent},
		[]string{firstConv, secondConv, childConv})
	insertStampChild(t, pool, firstParent, "", &firstConv, false)
	insertStampChild(t, pool, secondParent, "", &secondConv, false)
	insertStampChild(t, pool, childID, firstParent, &childConv, false)

	ctrl := &Controller{pool: pool}
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "first stamp on an adopted conversation")
	got, _ := stampParentOf(t, pool, childConv)
	c.Eq(firstConv, got, "the adopted link must be untouched")

	unsetStampParent(t, pool, childConv)
	relabelStampChild(t, pool, childID, secondParent)
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "second stamp")
	got, _ = stampParentOf(t, pool, childConv)
	c.Eq("", got, "a conversation discovered already stamped must be memoized too; got %s", got)
}

// insertTopLevelStampChild writes the child row a top-level spawn makes: a
// conversation and no parent label at all.
func insertTopLevelStampChild(t *testing.T, pool *pgxpool.Pool, childID string, conversationID *string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels, conversation_id)
		 VALUES ($1, 'fundi', 'idle', now(), '{}'::jsonb, $2::uuid)`, childID, conversationID)
	assert.NewAborting(t).NoError(err, "insert top-level child %s", childID)
}

// queryCounter counts the SQL statements a pool issues, so a test can assert
// that a call costs no lookup rather than inferring it from a probe.
type queryCounter struct{ n atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	return ctx
}

func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *queryCounter) reset()     { q.n.Store(0) }
func (q *queryCounter) count() int { return int(q.n.Load()) }

// openCountingTestPool opens the test pool with a query counter installed,
// skipping without RAFIKI_TEST_DSN like openTestPool does.
func openCountingTestPool(t *testing.T) (*pgxpool.Pool, *queryCounter) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "open pool")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool, counter
}

// TestStampConversationParentParentlessChildIsMemoizedToo pins the second
// terminal leg of the memo: a child with no parent label has nothing to find,
// so its first row write pays the two lookups and later ones pay none. Without
// the memoization every such write repeats both queries for the child's whole
// lifetime.
func TestStampConversationParentParentlessChildIsMemoizedToo(t *testing.T) {
	c := assert.NewAborting(t)
	pool, counter := openCountingTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	childID := "c_" + uniq + "_memo_top"

	childConv := insertStampConversation(t, pool, childID)
	cleanupStampRows(t, pool, []string{childID}, []string{childConv})
	insertTopLevelStampChild(t, pool, childID, &childConv)

	ctrl := &Controller{pool: pool}
	counter.reset()
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "first stamp")
	c.True(counter.count() > 0, "the first stamp on a parentless child must look at the child row")

	before := counter.count()
	c.NoError(ctrl.stampConversationParent(context.Background(), childID), "second stamp")
	c.Eq(before, counter.count(), "a memoized parentless child must issue no lookups on the next write")

	got, _ := stampParentOf(t, pool, childConv)
	c.Eq("", got, "a parentless child's conversation must stay top level")
}

// insertStampConversationWithParent writes one conversation row already
// carrying its parent link, the shape a row stamped by an earlier daemon
// has when this daemon first sees it.
func insertStampConversationWithParent(t *testing.T, pool *pgxpool.Pool, extRef, parentConvID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref, parent_conversation_id)
		 VALUES ('test', 'server', $1, $2::uuid) RETURNING id::text`, extRef, parentConvID).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert stamped conversation for %s", extRef)
	return id
}
