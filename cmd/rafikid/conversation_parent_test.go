// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/protocol"

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
