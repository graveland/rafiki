// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"

	"github.com/multigres/testkit/assert"
)

// backfillTestEnv is a scratch database at the head of the chain with one empty
// conversation, so a test writes only the rows it is about. Every row is
// inserted as a legacy row (no tree columns), which is what the backfill sees.
func backfillTestEnv(t *testing.T) (context.Context, *pgxpool.Pool, string) {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "Migrate")
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','client') RETURNING id::text`).Scan(&convID), "create conversation")
	return ctx, pool, convID
}

func backfillInsertRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal int, role, content string, kind *string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind)
		VALUES ($1::uuid, $2, $3, $4::jsonb, $5)`, convID, ordinal, role, content, kind)
	assert.NewAborting(t).NoError(err, "insert message ordinal %d", ordinal)
}

// backfillRun walks one conversation in its own transaction, as migration 0057's
// step does, and returns the report.
func backfillRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) BackfillReport {
	t.Helper()
	c := assert.NewAborting(t)
	tx, err := pool.Begin(ctx)
	c.NoError(err, "begin")
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	report, err := BackfillConversationTree(ctx, tx, convID)
	c.NoError(err, "BackfillConversationTree")
	c.NoError(tx.Commit(ctx), "commit")
	return report
}

// backfillStoredRow is one message row as the backfill left it. Parent and
// derived_from are read back as ORDINALS, so an assertion can say "row 2's
// parent is row 1" instead of comparing uuids.
type backfillStoredRow struct {
	ordinal    int
	role       string
	content    []byte
	kind       *string
	parent     *int
	derived    *int
	hash       []byte
	version    *int16
	depth      *int32
	providerID *string
}

func backfillTestRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) []backfillStoredRow {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT m.ordinal, m.role, m.content::text, m.kind, p.ordinal, d.ordinal,
		       m.path_hash, m.hash_version, m.depth, m.provider_message_id
		  FROM conversations.conversation_message m
		  LEFT JOIN conversations.conversation_message p ON p.id = m.parent_id
		  LEFT JOIN conversations.conversation_message d ON d.id = m.derived_from_id
		 WHERE m.conversation_id = $1::uuid ORDER BY m.ordinal`, convID)
	c.NoError(err, "read rows")
	defer rows.Close()
	var out []backfillStoredRow
	for rows.Next() {
		var row backfillStoredRow
		c.NoError(rows.Scan(&row.ordinal, &row.role, &row.content, &row.kind, &row.parent, &row.derived,
			&row.hash, &row.version, &row.depth, &row.providerID), "scan row")
		out = append(out, row)
	}
	c.NoError(rows.Err(), "rows")
	return out
}

func backfillTestHead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) *int {
	t.Helper()
	var head *int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx, `
		SELECT h.ordinal FROM conversations.conversation c
		  LEFT JOIN conversations.conversation_message h ON h.id = c.head_id
		 WHERE c.id = $1::uuid`, convID).Scan(&head), "read head ordinal")
	return head
}

// TestBackfillLinearChain: an ordinary pre-tree conversation becomes a chain —
// parents, depths, head, and hashes that are exactly convtree's.
func TestBackfillLinearChain(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	msgs := []struct{ role, content string }{
		{"user", `[{"type":"text","text":"one"}]`},
		{"assistant", `[{"type":"text","text":"two"}]`},
		{"user", `[{"type":"text","text":"three"}]`},
		{"assistant", `[{"type":"text","text":"four"}]`},
	}
	for i, m := range msgs {
		backfillInsertRow(t, ctx, pool, convID, i, m.role, m.content, nil)
	}

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(1, report.Conversations, "conversations")
	c.Eq(4, report.Nodes, "nodes")
	c.Eq(0, report.InterimCopies, "interim copies")
	c.Eq(0, report.UnmatchedCopies, "unmatched copies")
	c.Eq(0, report.OtherDuplicates, "other duplicates")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.Len(rows, 4, "rows")
	c.Nil(rows[0].parent, "the root has no parent")
	c.Nil(rows[0].derived, "a plain root is derived from nothing")
	for i := 1; i < len(rows); i++ {
		c.Require().NotNil(rows[i].parent, "row %d has a parent", i)
		c.Eq(i-1, *rows[i].parent, "row %d's parent", i)
	}
	for i := range rows {
		c.Require().NotNil(rows[i].depth, "row %d's depth", i)
		c.Eq(int32(i), *rows[i].depth, "row %d's depth", i)
		c.Require().NotNil(rows[i].version, "row %d's hash version", i)
		c.Eq(convtree.HashVersion, *rows[i].version, "row %d's hash version", i)
	}

	chain := make([]convtree.Msg, len(rows))
	for i := range rows {
		chain[i] = convtree.Msg{Role: rows[i].role, Content: rows[i].content}
	}
	want, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil), chain)
	c.NoError(err, "Chain")
	for i := range want {
		c.EqDeep(want[i][:], rows[i].hash, "row %d's path hash", i)
	}

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(3, *head, "head is the last row")
}

// TestBackfillCompactionRootAndRealTail: a compaction boundary starts a new
// root derived from the row it replaced, and the tail copies fundi appends after
// it are ordinary children — not interim-fork copies.
func TestBackfillCompactionRootAndRealTail(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	summary := KindCompactionSummary
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"summary"}]`, &summary)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 4, "assistant", `[{"type":"text","text":"b"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 5, "user", `[{"type":"text","text":"c"}]`, nil)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(0, report.InterimCopies, "the tail of a real compaction is not an interim copy")
	c.Eq(0, report.UnmatchedCopies, "unmatched copies")
	c.Eq(6, report.Nodes, "every row is a node")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.Nil(rows[2].parent, "the summary is a root")
	c.Require().NotNil(rows[2].derived, "the summary names what it replaced")
	c.Eq(1, *rows[2].derived, "the summary is derived from row 1")
	c.Eq(int32(0), *rows[2].depth, "the summary is at depth 0")
	for i, want := range map[int]int{3: 2, 4: 3, 5: 4} {
		c.Require().NotNil(rows[i].parent, "row %d has a parent", i)
		c.Eq(want, *rows[i].parent, "row %d's parent", i)
	}
	c.Eq(int32(3), *rows[5].depth, "row 5's depth")

	// The tail copies are the same content as rows 0 and 1 but sit under a
	// different parent slot, so they are different nodes with different hashes.
	c.False(bytes.Equal(rows[0].hash, rows[3].hash), "a tail copy under the summary is a new node")

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(5, *head, "head is the last row")
}

// TestBackfillClearRoot: a clear boundary starts a root too, derived from the
// row it cut away from.
func TestBackfillClearRoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	clearKind := KindClear
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"fresh start"}]`, &clearKind)
	backfillInsertRow(t, ctx, pool, convID, 3, "assistant", `[{"type":"text","text":"c"}]`, nil)

	backfillRun(t, ctx, pool, convID)
	rows := backfillTestRows(t, ctx, pool, convID)
	c.Nil(rows[2].parent, "the clear is a root")
	c.Require().NotNil(rows[2].derived, "the clear names what it cut from")
	c.Eq(1, *rows[2].derived, "the clear is derived from row 1")
	c.Require().NotNil(rows[3].parent, "row 3 has a parent")
	c.Eq(2, *rows[3].parent, "row 3 sits under the clear")
	c.Eq(int32(1), *rows[3].depth, "row 3's depth")
}

// TestBackfillTwoClearsAreDistinctRoots: a clear boundary is a constant, so two
// clears with identical content are told apart only by the row each was cut
// from. Without the derived-from seed they would collide into one node and the
// second clear would silently vanish from the tree.
func TestBackfillTwoClearsAreDistinctRoots(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	clearKind := KindClear
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"fresh start"}]`, &clearKind)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"c"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 4, "user", `[{"type":"text","text":"fresh start"}]`, &clearKind)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(0, report.OtherDuplicates, "the two clears are distinct nodes")
	c.Eq(5, report.Nodes, "nodes")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.False(bytes.Equal(rows[2].hash, rows[4].hash), "two clears with equal content are different nodes")
	c.Nil(rows[4].parent, "the second clear is a root")
	c.Eq(3, *rows[4].derived, "the second clear is derived from row 3")
}

// TestBackfillInterimForkCopiesAreDuplicates: a request rewrite made capture
// re-store the replayed request prefix, its unchanged part tagged
// compaction_tail. Those rows duplicate nodes that already exist, so they stay
// legacy rows, and the row that caused the fork becomes a sibling of the row it
// replaced.
func TestBackfillInterimForkCopiesAreDuplicates(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "assistant", `[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 4, "assistant", `[{"type":"text","text":"b"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 5, "assistant", `[{"type":"text","text":"No response requested."}]`, nil)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(2, report.InterimCopies, "interim copies")
	c.Eq(0, report.UnmatchedCopies, "unmatched copies")
	c.Eq(4, report.Nodes, "the copies are not nodes")

	rows := backfillTestRows(t, ctx, pool, convID)
	for _, i := range []int{3, 4} {
		c.Nil(rows[i].parent, "copy at ordinal %d has no parent", i)
		c.Nil(rows[i].hash, "copy at ordinal %d has no path hash", i)
		c.Nil(rows[i].depth, "copy at ordinal %d has no depth", i)
		c.Nil(rows[i].version, "copy at ordinal %d has no hash version", i)
	}
	c.Require().NotNil(rows[5].parent, "the rewrite has a parent")
	c.Eq(1, *rows[5].parent, "the rewrite is a sibling of row 2")
	c.Eq(int32(2), *rows[5].depth, "the rewrite's depth")

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(5, *head, "head is the rewrite")
}

// TestBackfillUnmatchedInterimCopyBecomesNode: a compaction_tail row that
// duplicates nothing is a node, and it is counted — an unexplained copy is the
// one shape that must never pass silently.
func TestBackfillUnmatchedInterimCopyBecomesNode(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"NOT a copy"}]`, &tail)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(1, report.UnmatchedCopies, "unmatched copies")
	c.Eq(0, report.InterimCopies, "interim copies")
	c.Eq(3, report.Nodes, "nodes")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.Require().NotNil(rows[2].parent, "the unmatched copy is a node")
	c.Eq(1, *rows[2].parent, "it hangs off the cursor")
	c.Require().NotNil(rows[2].hash, "it has a path hash")
}

// TestBackfillProviderMessageID: an assistant row a turn names gets that turn's
// reply id, so a capture can find the reply it stored.
func TestBackfillProviderMessageID(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"c"}]`, nil)
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn (conversation_id, ordinal, status, response_ordinal, response_message_id)
		VALUES ($1::uuid, 0, 'complete', 1, 'msg_reply_1')`, convID)
	c.NoError(err, "insert turn")

	backfillRun(t, ctx, pool, convID)
	rows := backfillTestRows(t, ctx, pool, convID)
	c.Require().NotNil(rows[1].providerID, "the assistant row carries the reply id")
	c.Eq("msg_reply_1", *rows[1].providerID, "provider message id")
	c.Nil(rows[0].providerID, "a user row carries no reply id")
	c.Nil(rows[2].providerID, "a row no turn names carries no reply id")
}

// interimSnapshot is the committed fixture: a real claude session replayed
// through the pre-tree capture code (pkg/capture's TestWriteInterimCaptureSnapshot).
type interimSnapshot struct {
	ResumeFromOrdinal *int                  `json:"resume_from_ordinal"`
	Messages          []interimSnapshotRow  `json:"messages"`
	Turns             []interimSnapshotTurn `json:"turns"`
}

type interimSnapshotRow struct {
	Ordinal     int             `json:"ordinal"`
	Role        string          `json:"role"`
	Content     json.RawMessage `json:"content"`
	Kind        *string         `json:"kind"`
	InputTokens *int64          `json:"input_tokens"`
	StopReason  *string         `json:"stop_reason"`
}

type interimSnapshotTurn struct {
	ResponseOrdinal   int    `json:"response_ordinal"`
	ResponseMessageID string `json:"response_message_id"`
}

// loadInterimSnapshot reads the committed fixture: a real claude session as the
// pre-tree capture path stored it.
func loadInterimSnapshot(t *testing.T) interimSnapshot {
	t.Helper()
	c := assert.NewAborting(t)
	raw, err := os.ReadFile(filepath.Join("..", "capture", "testdata", "interim-capture-rows.json"))
	c.Require().NoError(err, "read the interim snapshot")
	var snapshot interimSnapshot
	c.Require().NoError(json.Unmarshal(raw, &snapshot), "decode the interim snapshot")
	return snapshot
}

// TestBackfillSnapshotFixture is the backfill against real shapes: the rows
// capture actually wrote for a claude session with tool calls, a killed and
// resumed turn, a /compact and a /clear. It runs through Migrate, so step 56 —
// not a test-only call — is what does the work, and CheckTree is what judges it.
func TestBackfillSnapshotFixture(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)

	snapshot := loadInterimSnapshot(t)
	c.Greater(0, len(snapshot.Messages), "the snapshot has messages")

	// Stop one version short of the tree columns, write the legacy rows, then
	// let the chain — 0057's SQL and its go step together — place them.
	c.Require().NoError(migrateTo(ctx, pool, 55), "migrate to 55")
	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref, resume_from_ordinal)
		VALUES ('claude','client','interim-snapshot',$1) RETURNING id::text`,
		snapshot.ResumeFromOrdinal).Scan(&convID), "create conversation")
	for _, m := range snapshot.Messages {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_message
				(conversation_id, ordinal, role, content, kind, input_tokens, stop_reason)
			VALUES ($1::uuid, $2, $3, $4::jsonb, $5, $6, $7)`,
			convID, m.Ordinal, m.Role, []byte(m.Content), m.Kind, m.InputTokens, m.StopReason)
		c.Require().NoError(err, "insert message %d", m.Ordinal)
	}
	for _, turn := range snapshot.Turns {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_turn
				(conversation_id, ordinal, status, request, response_ordinal, response_message_id)
			VALUES ($1::uuid, 0, 'complete', '{}'::jsonb, $2, $3)`,
			convID, turn.ResponseOrdinal, turn.ResponseMessageID)
		c.Require().NoError(err, "insert turn")
	}

	c.Require().NoError(Migrate(ctx, pool), "Migrate")

	// The step's own report is not returned by Migrate, so walk again: the walk
	// is deterministic, and this is the number the rehearsal is judged on.
	report := backfillRun(t, ctx, pool, convID)
	t.Logf("snapshot backfill: %+v", report)
	c.Eq(0, report.UnmatchedCopies, "every interim-fork copy matched a node already placed")
	c.Greater(0, report.InterimCopies, "the fixture contains interim-fork copies")
	c.Greater(0, report.Nodes, "nodes")

	// Every compaction_tail row in this session is an interim-fork copy: the
	// /compact is followed by real rows, not by a fundi tail.
	var copies int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id = $1::uuid AND kind = $2`, convID, KindCompactionTail).Scan(&copies), "count tail rows")
	c.Eq(copies, report.InterimCopies, "every compaction_tail row is an interim copy")
	c.Eq(len(snapshot.Messages)-copies, report.Nodes, "every other row is a node")

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(len(snapshot.Messages)-1, *head, "head is the last row")

	check, err := checkTree(ctx, pool, []string{convID})
	c.Require().NoError(err, "checkTree")
	c.True(check.OK(), "tree check over the fixture: %+v", check)
}

// TestBackfillIsIdempotent: re-running the walk over rows it already placed
// assigns the same values, which is what makes a re-run after a partial cutover
// safe.
func TestBackfillIsIdempotent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"c"}]`, nil)

	first := backfillRun(t, ctx, pool, convID)
	before := backfillTestRows(t, ctx, pool, convID)
	second := backfillRun(t, ctx, pool, convID)
	after := backfillTestRows(t, ctx, pool, convID)
	c.EqDeep(first, second, "the same walk twice")
	c.EqDeep(before, after, "the same tree twice")
}

// TestBackfillSameMessage pins the identity rule the backfill and the check
// share. Its only leniency is a ZERO-VALUED field that one side omits — the
// noise the SDK's marshalling adds — and every subtest here is a clause of that
// sentence.
func TestBackfillSameMessage(t *testing.T) {
	t.Run("SDK union noise is the same message", func(t *testing.T) {
		c := assert.NewAborting(t)
		// One side is what capture stored (every union field, zero-valued when
		// unused, plus a provider marker); the other is the client's replay.
		stored := `[{"type":"thinking","thinking":"","signature":"sig","id":"","name":"","text":"","data":"","tool_use_id":"","content":{"type":"web_search_tool_result_error","error_code":""}},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"},"caller":{"type":"direct"}}]`
		replay := `[{"type":"thinking","thinking":"","signature":"sig"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"}}]`
		c.True(SameMessage(convtree.Msg{Role: "assistant", Content: []byte(stored)}, convtree.Msg{Role: "assistant", Content: []byte(replay)}), "zero-valued union noise is not a difference")

		// An empty list and a moved cache_control are the same kind of noise.
		c.True(SameMessage(
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"hi","citations":[]}]`)},
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]`)}),
			"an empty list and a transport hint are not content")
	})

	t.Run("a tool result's content is a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.False(SameMessage(
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)},
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"bad"}]`)}),
			"ok and bad are different tool output")
	})

	t.Run("is_error on one side only is a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.False(SameMessage(
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":true}]`)},
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)}),
			"a failed tool call is not a successful one")
	})

	t.Run("an image's source is a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.False(SameMessage(
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]`)},
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBB"}}]`)}),
			"two images are not one image")
	})

	t.Run("a non-zero field on one side only is a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.False(SameMessage(
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"hi","citations":[{"url":"x"}]}]`)},
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"hi"}]`)}),
			"a citation one side carries is content")
	})

	t.Run("a one-sided type tag with no payload is not a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		// The tag is ignored whatever it names — not only "direct": an object that
		// carries nothing but a type tag has no payload, so the tag names nothing
		// two messages can disagree about.
		c.True(SameMessage(
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"},"caller":{"type":"code_execution"}}]`)},
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"}}]`)}),
			"a tag alone is not content")
		// A tag that carries a payload is a difference, like any other non-zero
		// one-sided field.
		c.False(SameMessage(
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"},"caller":{"type":"code_execution","tool_id":"c1"}}]`)},
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x"}}]`)}),
			"a tag with a payload is content")
	})

	t.Run("a different role is a difference", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.False(SameMessage(
			convtree.Msg{Role: "user", Content: []byte(`[{"type":"text","text":"hi"}]`)},
			convtree.Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"hi"}]`)}),
			"role is part of the message")
	})
}

// TestBackfillRootResetsThePath: a boundary starts a chain of its own, so an
// interim run that follows one is measured against the new root — not merged
// onto nodes from before the boundary, which would put the replay's copies on
// the wrong branch.
func TestBackfillRootResetsThePath(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	summary := KindCompactionSummary
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "user", `[{"type":"text","text":"the summary"}]`, &summary)
	// A real row after the summary ends its tail, so the compaction_tail rows
	// that follow are a replay's copies — of nodes from BEFORE the boundary.
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"c"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 4, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 5, "assistant", `[{"type":"text","text":"b"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 6, "user", `[{"type":"text","text":"d"}]`, nil)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(0, report.InterimCopies, "the copies match nothing under the summary")
	c.Eq(2, report.UnmatchedCopies, "both copies of the run are unexplained")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.Nil(rows[2].parent, "the summary is a root")
	c.Require().NotNil(rows[4].parent, "the first copy became a node")
	c.Eq(3, *rows[4].parent, "it hangs off the post-boundary chain, not off the pre-boundary nodes")
	c.Eq(int32(2), *rows[4].depth, "its depth")
	c.Require().NotNil(rows[5].parent, "the second copy became a node")
	c.Eq(4, *rows[5].parent, "under the first")
	c.Require().NotNil(rows[6].parent, "the row after the run became a node")
	c.Eq(5, *rows[6].parent, "under the second")
	c.Eq(int32(4), *rows[6].depth, "its depth")

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(6, *head, "head is the last row")
}

// TestBackfillDuplicateSummaryKeepsItsTailReal: a repeated compaction summary is
// the same node, and it opens the same boundary, so the copies after it are
// fundi's real tail rather than a replay's leftovers.
func TestBackfillDuplicateSummaryKeepsItsTailReal(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	summary := KindCompactionSummary
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "user", `[{"type":"text","text":"the summary"}]`, &summary)
	backfillInsertRow(t, ctx, pool, convID, 2, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"the summary"}]`, &summary)
	backfillInsertRow(t, ctx, pool, convID, 4, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 5, "user", `[{"type":"text","text":"d"}]`, nil)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(1, report.OtherDuplicates, "the second summary is the same node")
	c.Eq(0, report.InterimCopies, "the tail is not an interim copy")
	c.Eq(0, report.UnmatchedCopies, "unmatched copies")

	rows := backfillTestRows(t, ctx, pool, convID)
	c.Nil(rows[3].hash, "the duplicate summary holds no position")
	c.Require().NotNil(rows[4].parent, "the tail copy is a node")
	c.Eq(1, *rows[4].parent, "it hangs off the summary the duplicate re-stated")
	c.Require().NotNil(rows[5].parent, "the next row is a node")
	c.Eq(4, *rows[5].parent, "under the tail")
}

// TestBackfillTwoInterimRunsInSequence: each run is matched against the path as
// it stood when that run began, so a second fork later in the conversation
// merges onto the second fork's prefix — never onto the first run's.
func TestBackfillTwoInterimRunsInSequence(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, convID := backfillTestEnv(t)
	tail := KindCompactionTail
	backfillInsertRow(t, ctx, pool, convID, 0, "user", `[{"type":"text","text":"a"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 1, "assistant", `[{"type":"text","text":"b"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 2, "assistant", `[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 3, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 4, "assistant", `[{"type":"text","text":"b"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 5, "assistant", `[{"type":"text","text":"first rewrite"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 6, "assistant", `[{"type":"text","text":"d"}]`, nil)
	backfillInsertRow(t, ctx, pool, convID, 7, "user", `[{"type":"text","text":"a"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 8, "assistant", `[{"type":"text","text":"b"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 9, "assistant", `[{"type":"text","text":"first rewrite"}]`, &tail)
	backfillInsertRow(t, ctx, pool, convID, 10, "user", `[{"type":"text","text":"second rewrite"}]`, nil)

	report := backfillRun(t, ctx, pool, convID)
	c.Eq(5, report.InterimCopies, "two copies in the first run, three in the second")
	c.Eq(0, report.UnmatchedCopies, "unmatched copies")

	rows := backfillTestRows(t, ctx, pool, convID)
	for _, ordinal := range []int{3, 4, 7, 8, 9} {
		c.Nil(rows[ordinal].hash, "copy at ordinal %d holds no position", ordinal)
	}
	c.Require().NotNil(rows[5].parent, "the first rewrite is a node")
	c.Eq(1, *rows[5].parent, "a sibling of the row it replaced")
	c.Require().NotNil(rows[10].parent, "the second rewrite is a node")
	c.Eq(5, *rows[10].parent, "a sibling of the first rewrite")

	head := backfillTestHead(t, ctx, pool, convID)
	c.Require().NotNil(head, "head")
	c.Eq(10, *head, "head is the second rewrite")
}
