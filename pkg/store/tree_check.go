// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"
)

// TreeCheckReport is the conversation tree's invariant check: every field must
// be zero (and no mismatch listed) for the tree to be a faithful copy of the
// history it was built from.
type TreeCheckReport struct {
	Conversations        int
	MissingHash          int      // rows with NULL path_hash that are not interim-fork compaction_tail rows
	BadDepth             int      // child.depth != parent.depth + 1
	MissingHead          int      // conversations with messages but NULL head_id
	OrdinalCounterBehind int      // next_ordinal <= max(ordinal)
	WorkingSetMismatch   []string // conversation ids whose working path content differs from the legacy working set
}

// OK reports whether every invariant holds.
func (r TreeCheckReport) OK() bool {
	return r.MissingHash == 0 && r.BadDepth == 0 && r.MissingHead == 0 &&
		r.OrdinalCounterBehind == 0 && len(r.WorkingSetMismatch) == 0
}

// CheckTree checks every conversation's tree invariants. It is the cutover's
// acceptance test: the backfill is judged by it, not by the fact that it ran.
func CheckTree(ctx context.Context, pool *pgxpool.Pool) (TreeCheckReport, error) {
	return checkTree(ctx, pool, nil)
}

// checkTree is CheckTree scoped to ids, or to every conversation when ids is
// nil. Tests scope it because the database they run against is shared.
func checkTree(ctx context.Context, pool *pgxpool.Pool, ids []string) (TreeCheckReport, error) {
	var report TreeCheckReport
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation c
		  WHERE ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))`, ids).Scan(&report.Conversations); err != nil {
		return report, fmt.Errorf("tree check: count conversations: %w", err)
	}
	counts := []struct {
		name   string
		query  string
		params []any
		into   *int
	}{
		{
			name: "missing hash",
			// A compaction_tail row may legitimately have no tree position: it
			// is a replay's copy of a node that does have one.
			query: `SELECT count(*) FROM conversations.conversation_message m
			         WHERE m.path_hash IS NULL AND m.kind IS DISTINCT FROM $2
			           AND ($1::uuid[] IS NULL OR m.conversation_id = ANY($1::uuid[]))`,
			params: []any{ids, KindCompactionTail},
			into:   &report.MissingHash,
		},
		{
			name: "bad depth",
			query: `SELECT count(*) FROM conversations.conversation_message m
			          JOIN conversations.conversation_message p ON p.id = m.parent_id
			         WHERE m.depth <> p.depth + 1
			           AND ($1::uuid[] IS NULL OR m.conversation_id = ANY($1::uuid[]))`,
			into: &report.BadDepth,
		},
		{
			name: "missing head",
			query: `SELECT count(*) FROM conversations.conversation c
			         WHERE c.head_id IS NULL
			           AND EXISTS (SELECT 1 FROM conversations.conversation_message m WHERE m.conversation_id = c.id)
			           AND ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))`,
			into: &report.MissingHead,
		},
		{
			name: "ordinal counter behind",
			query: `SELECT count(*) FROM conversations.conversation c
			         WHERE c.next_ordinal <= (SELECT max(m.ordinal) FROM conversations.conversation_message m
			                                   WHERE m.conversation_id = c.id)
			           AND ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))`,
			into: &report.OrdinalCounterBehind,
		},
	}
	for _, q := range counts {
		params := q.params
		if params == nil {
			params = []any{ids}
		}
		if err := pool.QueryRow(ctx, q.query, params...).Scan(q.into); err != nil {
			return report, fmt.Errorf("tree check: %s: %w", q.name, err)
		}
	}

	mismatches, err := checkWorkingSets(ctx, pool, ids)
	if err != nil {
		return report, err
	}
	report.WorkingSetMismatch = mismatches
	return report, nil
}

// checkWorkingSets compares every conversation's working path against the
// legacy working set it was built from: the rows at and after the resume
// horizon, in ordinal order.
//
// The comparison is on content, not on rows: an interim fork's copies are
// content-equal to the nodes they duplicate, so a forked conversation still
// compares equal — and that equality is exactly the property the tree relies on.
func checkWorkingSets(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT c.id::text FROM conversations.conversation c
		  WHERE ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))
		    AND EXISTS (SELECT 1 FROM conversations.conversation_message m WHERE m.conversation_id = c.id)
		  ORDER BY c.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("tree check: list conversations: %w", err)
	}
	var conversations []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("tree check: scan conversation: %w", err)
		}
		conversations = append(conversations, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tree check: list conversations: %w", err)
	}

	var mismatches []string
	for _, id := range conversations {
		working, err := checkLegacyWorkingSet(ctx, pool, id)
		if err != nil {
			return nil, err
		}
		path, err := checkWorkingPath(ctx, pool, id)
		if err != nil {
			return nil, err
		}
		if !checkSameMessages(working, path) {
			mismatches = append(mismatches, id)
		}
	}
	return mismatches, nil
}

// checkLegacyWorkingSet reads what the pre-tree LoadWorking returned: the rows
// at and after the resume horizon, in ordinal order, interim copies included.
func checkLegacyWorkingSet(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]convtree.Msg, error) {
	rows, err := pool.Query(ctx, `
		SELECT role, content::text FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid
		   AND ordinal >= coalesce((SELECT resume_from_ordinal FROM conversations.conversation WHERE id = $1::uuid), 0)
		 ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("tree check: read working set of %s: %w", conversationID, err)
	}
	return checkScanMessages(rows)
}

// checkWorkingPath reads the working path: the chain of nodes from the head back
// to its root, in root-to-head order.
func checkWorkingPath(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]convtree.Msg, error) {
	rows, err := pool.Query(ctx, `
		WITH RECURSIVE path AS (
		  SELECT m.id, m.parent_id, m.depth, m.role, m.content FROM conversations.conversation_message m
		    JOIN conversations.conversation c ON c.head_id = m.id WHERE c.id = $1::uuid
		  UNION ALL
		  SELECT p.id, p.parent_id, p.depth, p.role, p.content FROM conversations.conversation_message p JOIN path ON p.id = path.parent_id
		)
		SELECT role, content::text FROM path ORDER BY depth`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("tree check: read working path of %s: %w", conversationID, err)
	}
	return checkScanMessages(rows)
}

func checkScanMessages(rows pgx.Rows) ([]convtree.Msg, error) {
	defer rows.Close()
	var out []convtree.Msg
	for rows.Next() {
		var msg convtree.Msg
		if err := rows.Scan(&msg.Role, &msg.Content); err != nil {
			return nil, fmt.Errorf("tree check: scan message: %w", err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// checkSameMessages reports whether two histories are the same history: same
// length, and every message canonicalising equal to its counterpart.
func checkSameMessages(a, b []convtree.Msg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !convtree.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
