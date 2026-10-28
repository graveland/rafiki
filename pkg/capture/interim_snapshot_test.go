// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// updateInterimSnapshot arms TestWriteInterimCaptureSnapshot, the one-shot
// generator for testdata/interim-capture-rows.json.
var updateInterimSnapshot = flag.Bool("update-interim", false, "rewrite pkg/capture/testdata/interim-capture-rows.json from a replayed claude session")

// interimSnapshotPath is the committed snapshot the store's backfill test reads
// (as ../capture/testdata/interim-capture-rows.json).
var interimSnapshotPath = filepath.Join("testdata", "interim-capture-rows.json")

// interimSnapshot is a stored conversation as the pre-tree capture path wrote
// it: the rows a backfill must be able to place, and the turn→reply mapping
// that names an assistant row's provider message id.
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

// TestWriteInterimCaptureSnapshot regenerates testdata/interim-capture-rows.json
// by replaying the recorded claude session through the pre-tree capture code,
// the same way TestCapturedClaudeSessionReplays does. The result is what prod's
// conversation_message rows look like to the tree backfill — real boundary rows,
// real interim-fork copies — so the backfill is tested against shapes capture
// actually produced rather than against hand-built fixtures.
//
// It is a generator, not a test: skipped unless -update-interim is set.
func TestWriteInterimCaptureSnapshot(t *testing.T) {
	if !*updateInterimSnapshot {
		t.Skip("generator: pass -update-interim to rewrite the snapshot")
	}
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "interim-snapshot")

	dir := filepath.Join("..", "..", "testdata", "claude-capture")
	requests, err := filepath.Glob(filepath.Join(dir, "*-request.http"))
	c.NoError(err, "glob captured requests")
	c.Greater(0, len(requests), "no captured requests")

	var next int
	for n := 1; n <= len(requests); n++ {
		req := captureFixtureBody(t, dir, fmt.Sprintf("%06d-request.http", n))
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		c.NoError(json.Unmarshal(req, &body), "request %d body", n)
		if n > 1 && len(body.Messages) == 1 {
			c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending before request %d", n)
		}

		turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, Protocol: "anthropic"})
		c.NoError(err, "InsertTurnIntent request %d", n)
		next, err = s.DecomposeRequest(ctx, convID, turnID, createdAt, req, "")
		c.NoError(err, "DecomposeRequest request %d", n)

		_, _, canonical, _, err := routing.ParseCapturedResponse("text/event-stream", captureFixtureBody(t, dir, fmt.Sprintf("%06d-response.http", n)))
		c.NoError(err, "ParseCapturedResponse response %d", n)
		c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 1, 1, "end_turn"), "AppendResponseMessage response %d", n)
	}

	snapshot := interimSnapshot{
		ResumeFromOrdinal: readSnapshotHorizon(t, ctx, pool, convID),
		Messages:          readSnapshotRows(t, ctx, pool, convID),
		Turns:             readSnapshotTurns(t, ctx, pool, convID),
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	c.NoError(err, "marshal snapshot")
	c.NoError(os.MkdirAll(filepath.Dir(interimSnapshotPath), 0o755), "create testdata")
	c.NoError(os.WriteFile(interimSnapshotPath, append(encoded, '\n'), 0o644), "write %s", interimSnapshotPath)
	t.Logf("wrote %s: %d messages, %d turns, horizon %v, last ordinal %d",
		interimSnapshotPath, len(snapshot.Messages), len(snapshot.Turns), snapshot.ResumeFromOrdinal, next)
}

func readSnapshotHorizon(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) *int {
	t.Helper()
	var h *int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT resume_from_ordinal FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&h), "read resume_from_ordinal")
	return h
}

func readSnapshotRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) []interimSnapshotRow {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT ordinal, role, content, kind, input_tokens, stop_reason
		  FROM conversations.conversation_message WHERE conversation_id=$1::uuid ORDER BY ordinal`, convID)
	c.NoError(err, "read snapshot rows")
	defer rows.Close()
	var out []interimSnapshotRow
	for rows.Next() {
		var (
			row     interimSnapshotRow
			content []byte
		)
		c.NoError(rows.Scan(&row.Ordinal, &row.Role, &content, &row.Kind, &row.InputTokens, &row.StopReason), "scan snapshot row")
		row.Content = json.RawMessage(content)
		out = append(out, row)
	}
	c.NoError(rows.Err(), "snapshot rows")
	return out
}

func readSnapshotTurns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) []interimSnapshotTurn {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT response_ordinal, response_message_id
		  FROM conversations.conversation_turn
		 WHERE conversation_id=$1::uuid AND response_ordinal IS NOT NULL AND response_message_id IS NOT NULL
		 ORDER BY created_at`, convID)
	c.NoError(err, "read snapshot turns")
	defer rows.Close()
	var out []interimSnapshotTurn
	for rows.Next() {
		var turn interimSnapshotTurn
		c.NoError(rows.Scan(&turn.ResponseOrdinal, &turn.ResponseMessageID), "scan snapshot turn")
		out = append(out, turn)
	}
	c.NoError(rows.Err(), "snapshot turns")
	return out
}
