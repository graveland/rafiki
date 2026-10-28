// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"
)

// TreeCheckReport is the conversation tree's invariant check. Every count must
// be zero and no mismatch listed for the tree to be a faithful copy of the
// history it was built from; DuplicateRows and WorkingSetLooseMatch are
// informational and deliberately do not make a report fail.
type TreeCheckReport struct {
	Conversations int
	// MissingHash counts rows with no tree position that are neither an
	// interim-fork copy nor a duplicate of a node of the same conversation.
	MissingHash int
	// DuplicateRows counts rows whose content is already a node of this
	// conversation, so they hold no tree position. A compaction root's seed
	// ignores derived-from, so two summaries with the same content are one node
	// — the later row is a legitimate duplicate, not a missing position.
	DuplicateRows int
	// BadDepth counts children whose depth is not their parent's depth + 1.
	BadDepth int
	// MissingHead counts conversations with messages but no head_id.
	MissingHead int
	// OrdinalCounterBehind counts conversations whose next_ordinal is not
	// ahead of their highest ordinal.
	OrdinalCounterBehind int
	// WorkingSetMismatch lists conversations whose working path carries
	// different messages from the legacy working set it replaced.
	WorkingSetMismatch []string
	// WorkingSetLooseMatch lists conversations that match only under the looser
	// message identity the backfill used — the same message as the SDK models
	// it, not byte-for-byte. Informational: it does not fail the check, and it
	// is what a cutover rehearsal reads to see how much of the data relies on
	// the looser rule.
	WorkingSetLooseMatch []string
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
		name  string
		query string
		into  *int
	}{
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
		if err := pool.QueryRow(ctx, q.query, ids).Scan(q.into); err != nil {
			return report, fmt.Errorf("tree check: %s: %w", q.name, err)
		}
	}

	missing, duplicates, err := checkLegacyRows(ctx, pool, ids)
	if err != nil {
		return report, err
	}
	report.MissingHash = missing
	report.DuplicateRows = duplicates

	mismatches, loose, err := checkWorkingSets(ctx, pool, ids)
	if err != nil {
		return report, err
	}
	report.WorkingSetMismatch = mismatches
	report.WorkingSetLooseMatch = loose
	return report, nil
}

// checkNodeKeys is a conversation's nodes as the two identities a message can
// share with one: its canonical form, and the form the SDK models it in. They
// are the same two forms backfillSameMessage compares, as set keys.
type checkNodeKeys struct {
	strict map[string]struct{}
	loose  map[string]struct{}
}

func newCheckNodeKeys() checkNodeKeys {
	return checkNodeKeys{strict: map[string]struct{}{}, loose: map[string]struct{}{}}
}

func (k checkNodeKeys) add(msg convtree.Msg) {
	if canon, err := convtree.Canon(msg.Role, msg.Content); err == nil {
		k.strict[string(canon)] = struct{}{}
	}
	if form, ok := backfillCanonicalForm(msg); ok {
		k.loose[string(form)] = struct{}{}
	}
}

func (k checkNodeKeys) has(msg convtree.Msg) bool {
	if canon, err := convtree.Canon(msg.Role, msg.Content); err == nil {
		if _, ok := k.strict[string(canon)]; ok {
			return true
		}
	}
	if form, ok := backfillCanonicalForm(msg); ok {
		if _, ok := k.loose[string(form)]; ok {
			return true
		}
	}
	return false
}

// checkLegacyRows classifies the rows that hold no tree position. A row is a
// duplicate when a node of the same conversation carries the same message — the
// backfill's own notion of identity, so the check cannot disagree with it.
// Everything else without a position is missing.
func checkLegacyRows(ctx context.Context, pool *pgxpool.Pool, ids []string) (missing, duplicates int, err error) {
	conversations, err := checkConversationsWithLegacyRows(ctx, pool, ids)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range conversations {
		nodes, err := checkNodeKeysOf(ctx, pool, id)
		if err != nil {
			return 0, 0, err
		}
		legacy, err := checkLegacyMessages(ctx, pool, id)
		if err != nil {
			return 0, 0, err
		}
		for _, msg := range legacy {
			if nodes.has(msg) {
				duplicates++
				continue
			}
			missing++
		}
	}
	return missing, duplicates, nil
}

// checkConversationsWithLegacyRows lists the conversations holding a row with no
// tree position that is not an interim-fork copy. A compaction_tail row needs no
// position by construction, so it never reaches the duplicate test.
func checkConversationsWithLegacyRows(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT m.conversation_id::text FROM conversations.conversation_message m
		 WHERE m.path_hash IS NULL AND m.kind IS DISTINCT FROM $2
		   AND ($1::uuid[] IS NULL OR m.conversation_id = ANY($1::uuid[]))
		 ORDER BY 1`, ids, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("tree check: list conversations with legacy rows: %w", err)
	}
	return checkScanStrings(rows)
}

// checkNodeKeysOf reads the conversation's nodes — the rows that do hold a
// position — as the keys a duplicate is matched against.
func checkNodeKeysOf(ctx context.Context, pool *pgxpool.Pool, conversationID string) (checkNodeKeys, error) {
	keys := newCheckNodeKeys()
	rows, err := pool.Query(ctx, `
		SELECT role, content::text FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash IS NOT NULL`, conversationID)
	if err != nil {
		return keys, fmt.Errorf("tree check: read nodes of %s: %w", conversationID, err)
	}
	messages, err := checkScanMessages(rows)
	if err != nil {
		return keys, err
	}
	for _, msg := range messages {
		keys.add(msg)
	}
	return keys, nil
}

// checkLegacyMessages reads the conversation's rows that should have a position
// but do not.
func checkLegacyMessages(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]convtree.Msg, error) {
	rows, err := pool.Query(ctx, `
		SELECT role, content::text FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash IS NULL AND kind IS DISTINCT FROM $2
		 ORDER BY ordinal`, conversationID, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("tree check: read legacy rows of %s: %w", conversationID, err)
	}
	return checkScanMessages(rows)
}

// checkWorkingSets compares every conversation's working path against the
// legacy working set it was built from: the rows at and after the resume
// horizon, in ordinal order.
//
// The comparison is on messages, not on rows: an interim fork's copies are the
// same messages as the nodes they duplicate, so a forked conversation still
// compares equal — and that equality is exactly the property the tree relies on.
// A strict comparison runs first; a conversation that only matches under the
// looser identity the backfill used is reported separately, because "these rows
// are the same message in a different representation" is a fact a rehearsal
// wants to see rather than a failure.
func checkWorkingSets(ctx context.Context, pool *pgxpool.Pool, ids []string) (mismatches, loose []string, err error) {
	conversations, err := checkConversationsWithMessages(ctx, pool, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range conversations {
		working, err := checkLegacyWorkingSet(ctx, pool, id)
		if err != nil {
			return nil, nil, err
		}
		path, err := checkWorkingPath(ctx, pool, id)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case checkSameMessages(working, path):
		case checkSameMessagesLoose(working, path):
			loose = append(loose, id)
		default:
			mismatches = append(mismatches, id)
		}
	}
	return mismatches, loose, nil
}

func checkConversationsWithMessages(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT c.id::text FROM conversations.conversation c
		  WHERE ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))
		    AND EXISTS (SELECT 1 FROM conversations.conversation_message m WHERE m.conversation_id = c.id)
		  ORDER BY c.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("tree check: list conversations: %w", err)
	}
	return checkScanStrings(rows)
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

func checkScanStrings(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("tree check: scan id: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// checkSameMessages reports whether two histories are the same history: same
// length, and every message canonicalising equal to its counterpart.
func checkSameMessages(a, b []convtree.Msg) bool {
	return checkCompareMessages(a, b, convtree.Equal)
}

// checkSameMessagesLoose is the same comparison under the identity the backfill
// uses to decide two rows are one message.
func checkSameMessagesLoose(a, b []convtree.Msg) bool {
	return checkCompareMessages(a, b, backfillSameMessage)
}

func checkCompareMessages(a, b []convtree.Msg, same func(x, y convtree.Msg) bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !same(a[i], b[i]) {
			return false
		}
	}
	return true
}
