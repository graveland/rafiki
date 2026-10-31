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

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// updateInterimSnapshot arms TestWriteInterimCaptureSnapshot, the one-shot
// generator for testdata/interim-capture-rows.json.
var updateInterimSnapshot = flag.Bool("update-interim", false, "rewrite pkg/capture/testdata/interim-capture-rows.json from a replayed claude session")

// interimSnapshotPath is the committed snapshot the store's backfill test reads
// (as ../capture/testdata/interim-capture-rows.json).
var interimSnapshotPath = filepath.Join("testdata", "interim-capture-rows.json")

// interimSnapshot is a stored conversation in the shape the backfill reads: the
// rows to place, and the turn→reply mapping that names an assistant row's
// provider message id.
//
// The rows are a PRE-TREE capture's row stream — the thing the backfill exists
// to migrate — and a node's identity is the canon of its stored bytes, so the
// content must be the content capture writes TODAY or the backfill's parity
// test is comparing two different message identities.
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

// TestWriteInterimCaptureSnapshot refreshes testdata/interim-capture-rows.json:
// it replays the recorded claude session through the live capture path and
// rewrites each committed row's content with the content that path stores for
// the same message.
//
// Only the content is regenerated, never the row stream. A pre-tree capture
// stored a replayed request prefix again (as compaction_tail copies) whenever
// the client rewrote a message, and the backfill's interim-fork rule recovers a
// sibling from those copies; the tree capture stores the sibling directly and
// re-stores nothing, so its row stream is not a row stream a backfill can walk
// (see TestBackfillMatchesTreeCapture). The ordinals, roles and kinds are
// therefore the historical stream's, and each row's content is taken from the
// node carrying the same message — store.SameMessage, the identity the backfill
// itself matches copies with, which tolerates the SDK-union zero-valued fields
// the pre-tree capture wrote.
//
// It is a generator, not a test: skipped unless -update-interim is set. It
// refreshes the committed fixture rather than creating it (the stream cannot be
// rebuilt), and re-running it is a no-op once the fixture carries current
// content.
func TestWriteInterimCaptureSnapshot(t *testing.T) {
	if !*updateInterimSnapshot {
		t.Skip("generator: pass -update-interim to rewrite the snapshot")
	}
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)

	raw, err := os.ReadFile(interimSnapshotPath)
	c.NoError(err, "read the committed snapshot")
	var snapshot interimSnapshot
	c.NoError(json.Unmarshal(raw, &snapshot), "decode the committed snapshot")
	c.Greater(0, len(snapshot.Messages), "the committed snapshot has messages")

	// The same session, through the live capture path the proxy drives.
	convID, ref := clearTestConv(t, ctx, s, "interim-snapshot")
	fixtureReplay(t, ctx, s, convID, ref, nil)
	nodes := readSnapshotRows(t, ctx, pool, convID)
	c.Greater(0, len(nodes), "the replay stored nodes")

	// A pre-tree capture placed one row per message of the session, in order, and
	// tagged only the re-stored prefix copies compaction_tail. Those placed rows
	// are therefore the live path's nodes, one for one and in the same order, so
	// the i-th placed row carries the i-th node's content. Position, not content,
	// is the key: two nodes of this session carry messages that differ only in a
	// zero-valued field, which store.SameMessage calls equal but which hash
	// differently, so a content search cannot tell them apart.
	var placed []int
	for i := range snapshot.Messages {
		if k := snapshot.Messages[i].Kind; k == nil || *k != store.KindCompactionTail {
			placed = append(placed, i)
		}
	}
	if len(placed) != len(nodes) {
		t.Fatalf("the snapshot has %d placed rows, the replay stored %d nodes", len(placed), len(nodes))
	}
	for k, i := range placed {
		row := &snapshot.Messages[i]
		if !sameSnapshotMessage(*row, nodes[k]) {
			t.Fatalf("placed row ordinal %d (%s) is not replayed node ordinal %d", row.Ordinal, row.Role, nodes[k].Ordinal)
		}
		row.Content = nodes[k].Content
	}

	// A copy duplicates a node the session already placed. store.SameMessage is
	// the identity the backfill matches copies with, so a copy may take the
	// content of any node it matches — the walk's own cursor decides which node
	// it actually stands on.
	for i := range snapshot.Messages {
		row := &snapshot.Messages[i]
		if k := row.Kind; k == nil || *k != store.KindCompactionTail {
			continue
		}
		idx := -1
		for j, node := range nodes {
			if sameSnapshotMessage(*row, node) {
				idx = j
				break
			}
		}
		if idx < 0 {
			t.Fatalf("copy row ordinal %d (%s) duplicates no replayed node", row.Ordinal, row.Role)
		}
		row.Content = nodes[idx].Content
	}

	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	c.NoError(err, "marshal snapshot")
	c.NoError(os.MkdirAll(filepath.Dir(interimSnapshotPath), 0o755), "create testdata")
	c.NoError(os.WriteFile(interimSnapshotPath, append(encoded, '\n'), 0o644), "write %s", interimSnapshotPath)
	horizon := "null"
	if snapshot.ResumeFromOrdinal != nil {
		horizon = fmt.Sprint(*snapshot.ResumeFromOrdinal)
	}
	t.Logf("wrote %s: %d rows over %d replayed nodes, horizon %s",
		interimSnapshotPath, len(snapshot.Messages), len(nodes), horizon)
}

// sameSnapshotMessage reports whether a fixture row and a replayed node carry the
// same message, by the backfill's own identity: store.SameMessage tolerates the
// SDK-union zero-valued fields the pre-tree capture wrote on every block.
func sameSnapshotMessage(row, node interimSnapshotRow) bool {
	return store.SameMessage(
		convtree.Msg{Role: row.Role, Content: row.Content},
		convtree.Msg{Role: node.Role, Content: node.Content},
	)
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
