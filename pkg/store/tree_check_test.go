// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// checkTestConv creates an empty conversation in a scratch database at the head
// of the chain.
func checkTestConv(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var convID string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','client') RETURNING id::text`).Scan(&convID), "create conversation")
	return convID
}

// checkFakeHash is a distinct, deterministic path hash for a row that is not
// the subject of the test — the checker only needs the column populated.
func checkFakeHash(convID string, ordinal int) []byte {
	sum := sha256.Sum256([]byte(convID + ":" + strconv.Itoa(ordinal)))
	return sum[:]
}

// checkInsertRow writes one message row with its tree columns exactly as given,
// so a test can plant one specific defect. A nil hash leaves path_hash and
// hash_version NULL, as a legacy row has them.
func checkInsertRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal int, role, content string, kind *string, parentOrdinal *int, hash []byte, depth *int32) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message
			(conversation_id, ordinal, role, content, kind, parent_id, path_hash, hash_version, depth)
		VALUES ($1::uuid, $2, $3, $4::jsonb, $5,
		        (SELECT id FROM conversations.conversation_message
		          WHERE conversation_id = $1::uuid AND ordinal = $6),
		        $7, CASE WHEN $7::bytea IS NULL THEN NULL ELSE 1::smallint END, $8)`,
		convID, ordinal, role, content, kind, parentOrdinal, hash, depth)
	assert.NewAborting(t).NoError(err, "insert message ordinal %d", ordinal)
}

// checkSetConversation points the conversation at a head, an ordinal counter and
// a resume horizon; each is optional.
func checkSetConversation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, headOrdinal *int, nextOrdinal *int, horizon *int) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE conversations.conversation SET
			head_id = (SELECT id FROM conversations.conversation_message
			            WHERE conversation_id = $1::uuid AND ordinal = $2),
			next_ordinal = coalesce($3, next_ordinal),
			resume_from_ordinal = $4
		 WHERE id = $1::uuid`, convID, headOrdinal, nextOrdinal, horizon)
	assert.NewAborting(t).NoError(err, "set conversation")
}

func checkInt(v int) *int { return &v }

func checkDepth(v int32) *int32 { return &v }

// TestCheckTreeDetectsEachClass plants one defect per conversation — a row with
// no path hash, a child at the wrong depth, a conversation with no head, an
// ordinal counter behind the rows, and a working path that is not the working
// set — and asserts the check reports each of them, and only them.
func TestCheckTreeDetectsEachClass(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	// A: a legacy row with no path hash (not a compaction_tail copy). It sits
	// before the horizon, so it is not on the working path either.
	missingHash := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, missingHash, 0, "user", `[{"type":"text","text":"unhashed"}]`, nil, nil, nil, nil)
	checkInsertRow(t, ctx, pool, missingHash, 1, "user", `[{"type":"text","text":"head"}]`, nil, nil, checkFakeHash(missingHash, 1), checkDepth(0))
	checkSetConversation(t, ctx, pool, missingHash, checkInt(1), checkInt(2), checkInt(1))

	// B: a child one level deeper than its parent.
	badDepth := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, badDepth, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, checkFakeHash(badDepth, 0), checkDepth(0))
	checkInsertRow(t, ctx, pool, badDepth, 1, "assistant", `[{"type":"text","text":"child"}]`, nil, checkInt(0), checkFakeHash(badDepth, 1), checkDepth(5))
	checkSetConversation(t, ctx, pool, badDepth, checkInt(1), checkInt(2), checkInt(0))

	// C: messages, but no head. The horizon sits past the row, so the empty
	// working path matches the empty working set and only the head is wrong.
	missingHead := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, missingHead, 0, "user", `[{"type":"text","text":"orphan"}]`, nil, nil, checkFakeHash(missingHead, 0), checkDepth(0))
	checkSetConversation(t, ctx, pool, missingHead, nil, checkInt(1), checkInt(1))

	// D: the ordinal allocator would hand out an ordinal that is taken.
	counterBehind := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, counterBehind, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, checkFakeHash(counterBehind, 0), checkDepth(0))
	checkSetConversation(t, ctx, pool, counterBehind, checkInt(0), checkInt(0), checkInt(0))

	// E: the head is not the last row, so the working path is shorter than the
	// working set it must reproduce.
	mismatch := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, mismatch, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, checkFakeHash(mismatch, 0), checkDepth(0))
	checkInsertRow(t, ctx, pool, mismatch, 1, "assistant", `[{"type":"text","text":"child"}]`, nil, checkInt(0), checkFakeHash(mismatch, 1), checkDepth(1))
	checkSetConversation(t, ctx, pool, mismatch, checkInt(0), checkInt(2), checkInt(0))

	ids := []string{missingHash, badDepth, missingHead, counterBehind, mismatch}
	report, err := checkTree(ctx, pool, ids)
	c.Require().NoError(err, "checkTree")

	c.Eq(5, report.Conversations, "conversations checked")
	c.Eq(1, report.MissingHash, "missing hash")
	c.Eq(1, report.BadDepth, "bad depth")
	c.Eq(1, report.MissingHead, "missing head")
	c.Eq(1, report.OrdinalCounterBehind, "ordinal counter behind")
	c.EqDeep([]string{mismatch}, report.WorkingSetMismatch, "working set mismatch")
	c.False(report.OK(), "a report with defects is not OK")
}

// TestCheckTreeAcceptsACompactionTailCopy pins the one legitimate NULL path
// hash: a replay's copy of a node that does have one.
func TestCheckTreeAcceptsACompactionTailCopy(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	convID := checkTestConv(t, ctx, pool)
	tail := KindCompactionTail
	checkInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, checkFakeHash(convID, 0), checkDepth(0))
	// The fork's copy of the root, then the row that forked: the copy stands in
	// for the root on the working path, which is exactly why the comparison is
	// on content rather than on rows.
	checkInsertRow(t, ctx, pool, convID, 1, "user", `[{"type":"text","text":"root"}]`, &tail, nil, nil, nil)
	checkInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"after the fork"}]`, nil, checkInt(0), checkFakeHash(convID, 2), checkDepth(1))
	checkSetConversation(t, ctx, pool, convID, checkInt(2), checkInt(3), checkInt(1))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.Eq(0, report.MissingHash, "a tail copy needs no path hash")
	c.True(report.OK(), "report: %+v", report)
}

// TestCheckTreeAcceptsABackfilledTree is the positive control: the tree the
// backfill builds passes the check, so a failure over real data means the data
// is wrong rather than the checker.
func TestCheckTreeAcceptsABackfilledTree(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"c"}]`, nil)
	backfillRun(t, ctx, pool, convID)
	// A fork moves the resume horizon onto the copies, which is what makes the
	// working set and the working path the same length.
	_, err := pool.Exec(ctx, `UPDATE conversations.conversation
		SET next_ordinal = 4, resume_from_ordinal = 2 WHERE id = $1::uuid`, convID)
	c.Require().NoError(err, "seed next_ordinal and horizon")

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.True(report.OK(), "report: %+v", report)
	c.Eq(1, report.Conversations, "conversations checked")
}
