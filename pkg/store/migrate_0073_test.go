// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"
)

// TestMigrate0073BackfillsParentsAndActivity pins 0073: a thread conversation
// ('<session>:<thread>') points at its session's root under the same driven_by,
// a spawned child's root conversation points at its parent child's root through
// either parent label, a conversation that is neither stays top level, and
// updated_at becomes the newest stored message's time.
func TestMigrate0073BackfillsParentsAndActivity(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 72), "migrate to 0072, one short of 0073")

	// Fixtures, in the pre-0073 shape the backfill has to classify. Child ids
	// carry the real c_<ulid> shape because the migration reads them out of
	// labels and external_ref verbatim.
	newConversation := func(drivenBy string, externalRef *string) string {
		t.Helper()
		var id string
		c.Require().NoError(pool.QueryRow(ctx, `
			INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref)
			VALUES ('test', $1, $2) RETURNING id::text`, drivenBy, externalRef).Scan(&id),
			"insert the %s conversation", drivenBy)
		return id
	}
	str := func(s string) *string { return &s }

	child := func(childID, kind, labels string, conversationID any) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.child (child_id, kind, labels, status, spawned_at, conversation_id)
			VALUES ($1, $2, $3::jsonb, 'exited', now(), $4::uuid)`,
			childID, kind, labels, conversationID)
		c.Require().NoError(err, "insert child %s", childID)
	}

	// Parent fundi child P and its root conversation A: P names no parent, so A
	// must stay top level even though the child-roots arm walks it.
	convA := newConversation("server", nil)
	child("c_01PARENT", "fundi", `{}`, convA)

	// K names P by the current label and owns conversation B directly.
	convB := newConversation("server", nil)
	child("c_01CHILD", "fundi", `{"rafiki/parent": "c_01PARENT"}`, convB)

	// Q names P by the legacy label and owns no conversation row: the
	// claude-child shape, whose conversation links by external_ref = child id.
	convC := newConversation("client", str("c_01Q"))
	child("c_01Q", "claude", `{"fundi/parent": "c_01PARENT"}`, nil)

	// D is one of Q's threads: '<session>:<thread>' over the same driven_by.
	convD := newConversation("client", str("c_01Q:t9"))

	// E is an mcp:<user>:<id> ref: two colons, so it is no thread of anything.
	convE := newConversation("client", str("mcp:user:0f0e1d2c-3b4a-4c5d-8e9f-0a1b2c3d4e5f"))

	// Activity: F holds one message created now while its updated_at reads a
	// day old; G holds no message at all and must keep the updated_at it has.
	convF := newConversation("server", nil)
	_, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET updated_at = now() - interval '1 day' WHERE id = $1::uuid`,
		convF)
	c.Require().NoError(err, "age conv F")
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		VALUES ($1::uuid, 0, 'user', '[{"type":"text","text":"hi"}]'::jsonb)`, convF)
	c.Require().NoError(err, "insert conv F's message")
	convG := newConversation("server", nil)
	_, err = pool.Exec(ctx,
		`UPDATE conversations.conversation SET updated_at = now() - interval '2 hours' WHERE id = $1::uuid`,
		convG)
	c.Require().NoError(err, "age conv G")
	var gUpdatedAt time.Time
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT updated_at FROM conversations.conversation WHERE id = $1::uuid`, convG).Scan(&gUpdatedAt),
		"read conv G's updated_at")

	c.Require().NoError(Migrate(ctx, pool), "migrate 0073")

	parentOf := func(convID string) *string {
		t.Helper()
		var parent *string
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT parent_conversation_id::text FROM conversations.conversation WHERE id = $1::uuid`,
			convID).Scan(&parent), "read parent_conversation_id of %s", convID)
		return parent
	}

	parentEq := func(convID, want string, what string) {
		t.Helper()
		parent := parentOf(convID)
		c.Require().NotNil(parent, "%s must carry a parent", what)
		c.Eq(want, *parent, what)
	}
	parentNil := func(convID string, what string) {
		t.Helper()
		c.Nil(parentOf(convID), what)
	}

	parentEq(convB, convA, "a child's root conversation points at its parent child's root")
	parentEq(convC, convA, "a legacy fundi/parent label links the claude child's conversation too")
	parentEq(convD, convC, "a thread conversation points at its session's root")
	parentNil(convE, "an mcp external_ref is no thread and stays top level")
	parentNil(convA, "a child that names no parent stays top level")

	fUpdatedAt := updatedAtOf(t, pool, convF)
	c.True(fUpdatedAt.After(time.Now().Add(-time.Minute)),
		"conv F's updated_at = %s, want moved to its newest message", fUpdatedAt)
	c.Eq(gUpdatedAt, updatedAtOf(t, pool, convG), "a conversation with no messages keeps its updated_at")
}

// updatedAtOf reads a conversation's updated_at.
func updatedAtOf(t *testing.T, pool *pgxpool.Pool, convID string) time.Time {
	t.Helper()
	var at time.Time
	c := assert.NewAborting(t)
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT updated_at FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&at),
		"read updated_at of %s", convID)
	return at
}
