// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// forkTestSource creates a source conversation with n message rows and returns
// the conversation's id and the node ids in ordinal order. The rows are legacy
// linear rows (no path hash), which is all a fork edge needs. Everything the
// test writes is deleted again: the fork edges first, because they are ON
// DELETE RESTRICT against the message rows.
func forkTestSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore, n int) (string, []string) {
	t.Helper()
	c := assert.NewAborting(t)
	convID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "fork-test", DrivenBy: "server",
	})
	c.Require().NoError(err, "EnsureConversation (source)")

	ids := make([]string, 0, n)
	for i := range n {
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
			 VALUES ($1::uuid, $2, 'user', '[]'::jsonb) RETURNING id::text`,
			convID, i).Scan(&id)
		c.Require().NoError(err, "insert source row %d", i)
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.conversation
			WHERE fork_from IN (SELECT id FROM conversations.conversation_message WHERE conversation_id = $1::uuid)`,
			convID)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, convID)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.conversation WHERE id = $1::uuid`, convID)
	})
	return convID, ids
}

// forkRow reads a conversation's fork edge and next ordinal.
func forkRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) (string, int) {
	t.Helper()
	c := assert.NewAborting(t)
	var forkFrom *string
	var nextOrdinal int
	err := pool.QueryRow(ctx,
		`SELECT fork_from::text, next_ordinal FROM conversations.conversation WHERE id = $1::uuid`,
		convID).Scan(&forkFrom, &nextOrdinal)
	c.Require().NoError(err, "read fork row")
	if forkFrom == nil {
		return "", nextOrdinal
	}
	return *forkFrom, nextOrdinal
}

// TestEnsureConversationForkSetsEdgeAndOrdinal pins what creating a fork writes:
// the borrowed node as the edge, and that node's ordinal + 1 as the fork's
// allocator, so the fork's first row cannot collide with a borrowed one.
func TestEnsureConversationForkSetsEdgeAndOrdinal(t *testing.T) {
	ctx, pool, cs := captureTestEnv(t)
	c := assert.NewAborting(t)

	srcID, nodes := forkTestSource(t, ctx, pool, cs, 3)
	at := nodes[1]

	forkID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "fork-test", DrivenBy: "server", ForkFromNodeID: at,
	})
	c.Require().NoError(err, "EnsureConversation (fork)")

	forkFrom, nextOrdinal := forkRow(t, ctx, pool, forkID)
	c.Eq(at, forkFrom, "fork_from names the borrowed node")
	c.Eq(2, nextOrdinal, "next_ordinal follows the borrowed node's ordinal")

	// The source keeps no edge and no allocator of its own.
	forkFrom, nextOrdinal = forkRow(t, ctx, pool, srcID)
	c.Eq("", forkFrom, "the source is not a fork")
	c.Eq(0, nextOrdinal, "the source's allocator is untouched")

	// The same edge through the caller-supplied id.
	supplied := uuid.NewString()
	got, err := cs.EnsureConversation(ctx, ConversationRef{
		ID: supplied, OriginEntrypoint: "fork-test", DrivenBy: "server", ForkFromNodeID: nodes[2],
	})
	c.Require().NoError(err, "EnsureConversation (fork, supplied id)")
	c.Eq(supplied, got, "the supplied id is returned")
	forkFrom, nextOrdinal = forkRow(t, ctx, pool, got)
	c.Eq(nodes[2], forkFrom, "the edge is written for the supplied id")
	c.Eq(3, nextOrdinal, "and its ordinal follows that node")
}

// TestEnsureConversationForkFromUnknownNodeIsRefused pins that a fork of a node
// that is not stored inserts nothing and says so.
func TestEnsureConversationForkFromUnknownNodeIsRefused(t *testing.T) {
	ctx, pool, cs := captureTestEnv(t)
	c := assert.NewAborting(t)

	missing := uuid.NewString()
	id, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "fork-test", DrivenBy: "server", ForkFromNodeID: missing,
	})
	c.Require().Error(err, "an unknown node is refused")
	c.Eq("capture: fork-from node "+missing+" does not exist", err.Error(), "the refusal names the node")
	c.Eq("", id, "nothing is inserted")

	var count int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE fork_from = $1::uuid`, missing).Scan(&count),
		"count forks of the missing node")
	c.Eq(0, count, "no conversation row was written")

	// The caller-supplied-id branch refuses too, and writes nothing.
	supplied := uuid.NewString()
	_, err = cs.EnsureConversation(ctx, ConversationRef{
		ID: supplied, OriginEntrypoint: "fork-test", DrivenBy: "server", ForkFromNodeID: missing,
	})
	c.Require().Error(err, "an unknown node is refused with a supplied id")
	c.Eq("capture: fork-from node "+missing+" does not exist", err.Error(), "the refusal names the node")
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE id = $1::uuid`, supplied).Scan(&count),
		"count the supplied id")
	c.Eq(0, count, "no conversation row was written for the supplied id")
}

// TestEnsureConversationByExternalRefForkIsIgnoredOnResume pins that a fork's
// edge is written once: the second call for the same session, naming another
// node, leaves the first edge in place.
func TestEnsureConversationByExternalRefForkIsIgnoredOnResume(t *testing.T) {
	ctx, pool, cs := captureTestEnv(t)
	c := assert.NewAborting(t)

	_, nodes := forkTestSource(t, ctx, pool, cs, 3)
	ref := "fork-ext-" + uuid.NewString()

	id, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "fork-test", DrivenBy: "client", ExternalRef: ref, ForkFromNodeID: nodes[0],
	})
	c.Require().NoError(err, "EnsureConversationByExternalRef (fork)")

	forkFrom, nextOrdinal := forkRow(t, ctx, pool, id)
	c.Eq(nodes[0], forkFrom, "the first edge is written")
	c.Eq(1, nextOrdinal, "and its ordinal follows the first node")

	again, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "fork-test", DrivenBy: "client", ExternalRef: ref, ForkFromNodeID: nodes[2],
	})
	c.Require().NoError(err, "EnsureConversationByExternalRef (resume)")
	c.Eq(id, again, "the same session resolves to the same conversation")

	forkFrom, nextOrdinal = forkRow(t, ctx, pool, again)
	c.Eq(nodes[0], forkFrom, "the edge never changes on resume")
	c.Eq(1, nextOrdinal, "nor does the allocator")
}
