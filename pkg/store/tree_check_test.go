// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"

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

// checkForeignHash is a well-formed hash that is not the hash any position
// implies: what a corrupted or wrongly-written path_hash looks like.
func checkForeignHash(convID string, ordinal int) []byte {
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

// checkInsertNode writes one row that holds a tree position, computing the path
// hash that position requires — so the hash check has nothing to report and the
// row isolates whatever defect the test is about. parentHash is nil for a root.
func checkInsertNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal int, role, content string, kind *string, parentOrdinal *int, parentHash []byte, depth int32) []byte {
	t.Helper()
	var slot convtree.Hash
	if parentHash == nil {
		rootKind := convtree.RootKindRoot
		if kind != nil {
			rootKind = *kind
		}
		slot = convtree.RootSeed(rootKind, nil)
	} else {
		slot = convtree.Hash(parentHash)
	}
	hash, err := convtree.NodeHash(slot, convtree.Msg{Role: role, Content: []byte(content)})
	assert.NewAborting(t).NoError(err, "hash message ordinal %d", ordinal)
	checkInsertRow(t, ctx, pool, convID, ordinal, role, content, kind, parentOrdinal, hash[:], checkDepth(depth))
	return hash[:]
}

// checkInsertSnapshotNode is checkInsertNode over one of the committed
// fixture's own rows, so a test can build a real shape without inventing
// content.
func checkInsertSnapshotNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal, fixtureOrdinal int, kind *string, parentOrdinal *int, parentHash []byte, depth int32) []byte {
	t.Helper()
	row := loadInterimSnapshot(t).message(t, fixtureOrdinal)
	return checkInsertNode(t, ctx, pool, convID, ordinal, row.Role, string(row.Content), kind, parentOrdinal, parentHash, depth)
}

// checkInsertSnapshotRow writes one row of the committed fixture at the given
// ordinal with no tree position, which is what an interim-fork copy is.
func checkInsertSnapshotRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal, fixtureOrdinal int, kind *string, parentOrdinal *int, hash []byte, depth *int32) {
	t.Helper()
	row := loadInterimSnapshot(t).message(t, fixtureOrdinal)
	checkInsertRow(t, ctx, pool, convID, ordinal, row.Role, string(row.Content), kind, parentOrdinal, hash, depth)
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
// set — and asserts the check reports each of them, and only them. Every hash is
// the one its position implies, so the hash check has nothing to say.
func TestCheckTreeDetectsEachClass(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	// A: a legacy row with no path hash (not a compaction_tail copy, and no
	// repeated text, so it is not a duplicate). It sits before the horizon, so
	// it is not on the working path either.
	missingHash := checkTestConv(t, ctx, pool)
	checkInsertRow(t, ctx, pool, missingHash, 0, "user", `[{"type":"text","text":"unhashed"}]`, nil, nil, nil, nil)
	checkInsertNode(t, ctx, pool, missingHash, 1, "user", `[{"type":"text","text":"head"}]`, nil, nil, nil, 0)
	checkSetConversation(t, ctx, pool, missingHash, checkInt(1), checkInt(2), checkInt(1))

	// B: a child one level deeper than its parent.
	badDepth := checkTestConv(t, ctx, pool)
	rootHash := checkInsertNode(t, ctx, pool, badDepth, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 0)
	checkInsertNode(t, ctx, pool, badDepth, 1, "assistant", `[{"type":"text","text":"child"}]`, nil, checkInt(0), rootHash, 5)
	checkSetConversation(t, ctx, pool, badDepth, checkInt(1), checkInt(2), checkInt(0))

	// C: messages, but no head. The horizon sits past the row, so the empty
	// working path matches the empty working set and only the head is wrong.
	missingHead := checkTestConv(t, ctx, pool)
	checkInsertNode(t, ctx, pool, missingHead, 0, "user", `[{"type":"text","text":"orphan"}]`, nil, nil, nil, 0)
	checkSetConversation(t, ctx, pool, missingHead, nil, checkInt(1), checkInt(1))

	// D: the ordinal allocator would hand out an ordinal that is taken.
	counterBehind := checkTestConv(t, ctx, pool)
	checkInsertNode(t, ctx, pool, counterBehind, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 0)
	checkSetConversation(t, ctx, pool, counterBehind, checkInt(0), checkInt(0), checkInt(0))

	// E: the head is not the last row, so the working path is shorter than the
	// working set it must reproduce.
	mismatch := checkTestConv(t, ctx, pool)
	rootHash = checkInsertNode(t, ctx, pool, mismatch, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 0)
	checkInsertNode(t, ctx, pool, mismatch, 1, "assistant", `[{"type":"text","text":"child"}]`, nil, checkInt(0), rootHash, 1)
	checkSetConversation(t, ctx, pool, mismatch, checkInt(0), checkInt(2), checkInt(0))

	ids := []string{missingHash, badDepth, missingHead, counterBehind, mismatch}
	report, err := checkTree(ctx, pool, ids)
	c.Require().NoError(err, "checkTree")

	c.Eq(5, report.Conversations, "conversations checked")
	c.Eq(1, report.MissingHash, "missing hash")
	c.Eq(0, report.DuplicateRows, "duplicate rows")
	c.Eq(0, report.BadHash, "bad hash")
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
	rootHash := checkInsertNode(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 0)
	// The fork's copy of the root, then the row that forked: the copy stands in
	// for the root on the working path, which is exactly why the comparison is
	// on messages rather than on rows.
	checkInsertRow(t, ctx, pool, convID, 1, "user", `[{"type":"text","text":"root"}]`, &tail, nil, nil, nil)
	checkInsertNode(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"after the fork"}]`, nil, checkInt(0), rootHash, 1)
	checkSetConversation(t, ctx, pool, convID, checkInt(2), checkInt(3), checkInt(1))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.Eq(0, report.MissingHash, "a tail copy needs no path hash")
	c.True(report.OK(), "report: %+v", report)
}

// TestCheckTreeAcceptsABackfilledTree is the positive control: the tree the
// backfill builds passes every check, so a failure over real data means the data
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
	c.Eq(0, report.BadHash, "the backfill's own hashes verify")
	c.True(report.OK(), "report: %+v", report)
	c.Eq(1, report.Conversations, "conversations checked")
}

// TestCheckTreeLooseMatchForRepresentationOnlyCopy: a forked conversation whose
// copies differ from the originals only in how the message was written down —
// the SDK's zero-valued union fields capture stored, and a cache_control the
// client moved — is not a mismatch. It is the looser identity the backfill used,
// reported separately so a rehearsal can see how much of the data relies on it.
//
// Built from the fixture's own rows: 0 and 26 differ by cache_control alone, 1
// and 27 are the same reply as capture stored it and as the client replayed it.
func TestCheckTreeLooseMatchForRepresentationOnlyCopy(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	convID := checkTestConv(t, ctx, pool)
	tail := KindCompactionTail
	rootHash := checkInsertSnapshotNode(t, ctx, pool, convID, 0, 0, nil, nil, nil, 0)
	replyHash := checkInsertSnapshotNode(t, ctx, pool, convID, 1, 1, nil, checkInt(0), rootHash, 1)
	checkInsertSnapshotRow(t, ctx, pool, convID, 2, 26, &tail, nil, nil, nil)
	checkInsertSnapshotRow(t, ctx, pool, convID, 3, 27, &tail, nil, nil, nil)
	checkInsertSnapshotNode(t, ctx, pool, convID, 4, 52, nil, checkInt(1), replyHash, 2)
	checkSetConversation(t, ctx, pool, convID, checkInt(4), checkInt(5), checkInt(2))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.Empty(report.WorkingSetMismatch, "a representation-only copy is not a mismatch")
	c.EqDeep([]string{convID}, report.WorkingSetLooseMatch, "the conversation matches only under the looser identity")
	c.Eq(0, report.MissingHash, "copies need no path hash")
	c.Eq(0, report.DuplicateRows, "copies are not duplicates")
	c.Eq(0, report.BadHash, "hashes verify")
	c.True(report.OK(), "report: %+v", report)
}

// TestCheckTreeRejectsAGenuineCopyDifference: the looser identity is not
// permissive. A copy whose tool output actually changed is a mismatch — a
// one-sided non-zero field is a difference, never noise.
func TestCheckTreeRejectsAGenuineCopyDifference(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	convID := checkTestConv(t, ctx, pool)
	tail := KindCompactionTail
	original := loadInterimSnapshot(t).message(t, 4)
	rootHash := checkInsertSnapshotNode(t, ctx, pool, convID, 0, 4, nil, nil, nil, 0)
	replyHash := checkInsertSnapshotNode(t, ctx, pool, convID, 1, 5, nil, checkInt(0), rootHash, 1)
	checkInsertRow(t, ctx, pool, convID, 2, original.Role, checkTamperedContent(t, original.Content, "tool-ok", "tampered"), &tail, nil, nil, nil)
	checkInsertSnapshotRow(t, ctx, pool, convID, 3, 5, &tail, nil, nil, nil)
	checkInsertSnapshotNode(t, ctx, pool, convID, 4, 52, nil, checkInt(1), replyHash, 2)
	checkSetConversation(t, ctx, pool, convID, checkInt(4), checkInt(5), checkInt(2))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.EqDeep([]string{convID}, report.WorkingSetMismatch, "a changed tool output is a mismatch")
	c.Empty(report.WorkingSetLooseMatch, "it does not match under the looser identity either")
	c.False(report.OK(), "a mismatch fails the check")
}

// checkTamperedContent rewrites one string inside a message's JSON, the way a
// client that changed a message it had already sent would.
func checkTamperedContent(t *testing.T, content []byte, from, to string) string {
	t.Helper()
	if !bytes.Contains(content, []byte(from)) {
		t.Fatalf("fixture content does not contain %q", from)
	}
	return string(bytes.Replace(content, []byte(from), []byte(to), 1))
}

// TestCheckTreeCountsDuplicateRows: a compaction root's seed ignores
// derived-from, so a second summary with identical content is the SAME node. The
// row that re-states it holds no tree position by design and is counted as a
// duplicate, not as a missing position.
func TestCheckTreeCountsDuplicateRows(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	convID := checkTestConv(t, ctx, pool)
	summary := KindCompactionSummary
	clearKind := KindClear
	checkInsertNode(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil, nil, nil, 0)
	summaryHash := checkInsertNode(t, ctx, pool, convID, 1, "user", `[{"type":"text","text":"the summary"}]`, &summary, nil, nil, 0)
	checkInsertNode(t, ctx, pool, convID, 2, "assistant", `[{"type":"text","text":"b"}]`, nil, checkInt(1), summaryHash, 1)
	// The same summary again: the same node, so no position.
	checkInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"the summary"}]`, &summary, nil, nil, nil)
	checkInsertNode(t, ctx, pool, convID, 4, "user", `[{"type":"text","text":"fresh"}]`, &clearKind, nil, nil, 0)
	checkSetConversation(t, ctx, pool, convID, checkInt(4), checkInt(5), checkInt(4))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.Eq(1, report.DuplicateRows, "the repeated summary is a duplicate")
	c.Eq(0, report.MissingHash, "a duplicate is not a missing position")
	c.Eq(0, report.BadHash, "hashes verify")
	c.True(report.OK(), "report: %+v", report)
}

// TestCheckTreeMissingHashForARepeatedOrdinaryRow: only a root can be excused by
// content, because only a root's slot is content-addressed from its kind. An
// ordinary row whose text happens to repeat is still a missing position, and so
// is a boundary row that repeats a DIFFERENT kind's root — the check must stay
// loud rather than explain away a row it cannot account for.
func TestCheckTreeMissingHashForARepeatedOrdinaryRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	convID := checkTestConv(t, ctx, pool)
	summary := KindCompactionSummary
	clearKind := KindClear
	checkInsertNode(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"same words"}]`, nil, nil, nil, 0)
	checkInsertNode(t, ctx, pool, convID, 1, "user", `[{"type":"text","text":"the summary"}]`, &summary, nil, nil, 0)
	// An ordinary row with the plain root's exact content, holding no position.
	checkInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"same words"}]`, nil, nil, nil, nil)
	// A clear whose content repeats a compaction root's: the kind differs, so it
	// is not the same node.
	checkInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"the summary"}]`, &clearKind, nil, nil, nil)
	checkInsertNode(t, ctx, pool, convID, 4, "user", `[{"type":"text","text":"fresh"}]`, &clearKind, nil, nil, 0)
	checkSetConversation(t, ctx, pool, convID, checkInt(4), checkInt(5), checkInt(4))

	report, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.Eq(2, report.MissingHash, "an ordinary row and a different-kind boundary are both missing positions")
	c.Eq(0, report.DuplicateRows, "duplicate rows")
	c.False(report.OK(), "a missing position fails the check")
}

// TestCheckTreeDetectsABadHash: the check recomputes every node's hash from its
// position, so a tree that is well-shaped but addressed by the wrong hashes is
// caught. Each subtest plants one of the three states BadHash covers.
func TestCheckTreeDetectsABadHash(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	assert.NewAborting(t).Require().NoError(Migrate(ctx, pool), "Migrate")

	t.Run("corrupted hash", func(t *testing.T) {
		c := assert.NewAborting(t)
		convID := checkTestConv(t, ctx, pool)
		checkInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, checkForeignHash(convID, 0), checkDepth(0))
		checkSetConversation(t, ctx, pool, convID, checkInt(0), checkInt(1), checkInt(0))

		report, err := checkTree(ctx, pool, []string{convID})
		c.Require().NoError(err, "checkTree")
		c.Eq(1, report.BadHash, "a hash that is not the hash of its position")
		c.Eq(0, report.MissingHash, "it has a position")
		c.False(report.OK(), "a bad hash fails the check")
	})

	t.Run("parent in another conversation", func(t *testing.T) {
		c := assert.NewAborting(t)
		parentConv := checkTestConv(t, ctx, pool)
		checkInsertNode(t, ctx, pool, parentConv, 0, "user", `[{"type":"text","text":"elsewhere"}]`, nil, nil, nil, 0)
		convID := checkTestConv(t, ctx, pool)
		checkInsertNode(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 0)
		checkInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"child"}]`, nil, checkInt(0), checkForeignHash(convID, 1), checkDepth(1))
		_, err := pool.Exec(ctx, `
			UPDATE conversations.conversation_message SET parent_id = (
				SELECT id FROM conversations.conversation_message
				 WHERE conversation_id = $1::uuid AND ordinal = 0)
			 WHERE conversation_id = $2::uuid AND ordinal = 1`, parentConv, convID)
		c.Require().NoError(err, "point the child at another conversation's row")
		checkSetConversation(t, ctx, pool, convID, checkInt(1), checkInt(2), checkInt(0))

		report, err := checkTree(ctx, pool, []string{convID})
		c.Require().NoError(err, "checkTree")
		c.Eq(1, report.BadHash, "a parent outside the conversation")
		c.Eq(0, report.BadDepth, "its depth is arithmetically fine")
		c.False(report.OK(), "a foreign parent fails the check")
	})

	t.Run("root below depth zero", func(t *testing.T) {
		c := assert.NewAborting(t)
		convID := checkTestConv(t, ctx, pool)
		checkInsertNode(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"root"}]`, nil, nil, nil, 1)
		checkSetConversation(t, ctx, pool, convID, checkInt(0), checkInt(1), checkInt(0))

		report, err := checkTree(ctx, pool, []string{convID})
		c.Require().NoError(err, "checkTree")
		c.Eq(1, report.BadHash, "a root that is not at depth 0")
		c.False(report.OK(), "a root below depth zero fails the check")
	})
}
