// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
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
	// DuplicateRows counts rows whose content is already a compaction_summary
	// root of this conversation, so they hold no tree position. A root's parent
	// slot is convtree.RootSeed(kind, derivedFrom), which ignores derivedFrom for
	// every kind but clear — so for a summary, and only for a summary, equal
	// content under an equal kind is provably the same node, and the later row is
	// a legitimate duplicate rather than a missing position. Nothing else without
	// a position is excused: a clear's seed IS its derived-from, and a repeated
	// ordinary message has no seed of its own, so neither can be proven one node.
	DuplicateRows int
	// BadHash counts nodes whose stored hash is not the hash their position
	// implies, plus the two states that make a node's position meaningless: a
	// root that is not at depth 0, and a node whose parent is not a row of this
	// conversation (or has no position itself).
	BadHash int
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
	return r.MissingHash == 0 && r.BadHash == 0 && r.BadDepth == 0 && r.MissingHead == 0 &&
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

	badHashes, err := checkBadHashes(ctx, pool, ids)
	if err != nil {
		return report, err
	}
	report.BadHash = badHashes

	mismatches, loose, err := checkWorkingSets(ctx, pool, ids)
	if err != nil {
		return report, err
	}
	report.WorkingSetMismatch = mismatches
	report.WorkingSetLooseMatch = loose
	return report, nil
}

// checkRoot is a root of the conversation: a node with no parent.
type checkRoot struct {
	kind *string
	msg  convtree.Msg
}

// checkLegacyRow is a row that holds no tree position and is not an
// interim-fork copy.
type checkLegacyRow struct {
	kind *string
	msg  convtree.Msg
}

// checkLegacyRows classifies the rows that hold no tree position. A row is a
// duplicate only when it re-states a compaction_summary root — that kind's seed
// carries no derived-from component, so equal content under an equal kind is
// provably the same node. Every other row without a position is missing, which
// is loud on purpose: a clear's seed is the node it was cut from, and a repeated
// ordinary message has no seed at all, so neither is proven to be one node.
func checkLegacyRows(ctx context.Context, pool *pgxpool.Pool, ids []string) (missing, duplicates int, err error) {
	conversations, err := checkConversationsWithLegacyRows(ctx, pool, ids)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range conversations {
		roots, err := checkRootsOf(ctx, pool, id)
		if err != nil {
			return 0, 0, err
		}
		legacy, err := checkLegacyRowsOf(ctx, pool, id)
		if err != nil {
			return 0, 0, err
		}
		for _, row := range legacy {
			if checkRestatesARoot(roots, row) {
				duplicates++
				continue
			}
			missing++
		}
	}
	return missing, duplicates, nil
}

// checkRestatesARoot reports whether a row is a second copy of a compaction
// summary root of the same kind, which is the only case that is provable.
//
// A root's parent slot is convtree.RootSeed(kind, derivedFrom), and the seed
// ignores derivedFrom for every kind except clear. So a summary root's slot is
// its kind and nothing else: equal content under an equal kind IS the same node.
// A clear root's slot is the node it was cut from, and a clear's text is a
// constant, so two clears from different cursors are different nodes however
// equal their content — excusing one would be explaining away a row the backfill
// failed to place. A plain root's seed carries no kind of its own either, so two
// ordinary rows with equal content are two positions.
func checkRestatesARoot(roots []checkRoot, row checkLegacyRow) bool {
	if row.kind == nil || *row.kind != KindCompactionSummary {
		return false
	}
	for _, root := range roots {
		if root.kind == nil || *root.kind != *row.kind {
			continue
		}
		if SameMessage(root.msg, row.msg) {
			return true
		}
	}
	return false
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

// checkRootsOf reads the conversation's roots: the nodes with no parent.
func checkRootsOf(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]checkRoot, error) {
	rows, err := pool.Query(ctx, `
		SELECT kind, role, content::text FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND parent_id IS NULL AND path_hash IS NOT NULL`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("tree check: read roots of %s: %w", conversationID, err)
	}
	defer rows.Close()
	var out []checkRoot
	for rows.Next() {
		var root checkRoot
		if err := rows.Scan(&root.kind, &root.msg.Role, &root.msg.Content); err != nil {
			return nil, fmt.Errorf("tree check: scan root: %w", err)
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// checkLegacyRowsOf reads the conversation's rows that should have a position
// but do not.
func checkLegacyRowsOf(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]checkLegacyRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT kind, role, content::text FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash IS NULL AND kind IS DISTINCT FROM $2
		 ORDER BY ordinal`, conversationID, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("tree check: read legacy rows of %s: %w", conversationID, err)
	}
	defer rows.Close()
	var out []checkLegacyRow
	for rows.Next() {
		var row checkLegacyRow
		if err := rows.Scan(&row.kind, &row.msg.Role, &row.msg.Content); err != nil {
			return nil, fmt.Errorf("tree check: scan legacy row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// checkHashLen is the width of a path hash, which a bytea column does not
// enforce.
const checkHashLen = len(convtree.Hash{})

// checkTreeRow is one message row with the columns a hash check needs.
type checkTreeRow struct {
	id      string
	parent  *string
	derived *string
	kind    *string
	role    string
	content []byte
	hash    []byte
	version *int16
	depth   *int32
}

// checkBadHashes recomputes every node's path hash from its position and counts
// the ones that do not match. This is the check that makes the tree
// content-addressed rather than merely well-shaped: without it a tree with
// consistent parents, depths and heads can still be addressed by hashes that
// describe a different history.
func checkBadHashes(ctx context.Context, pool *pgxpool.Pool, ids []string) (int, error) {
	conversations, err := checkConversationsWithMessages(ctx, pool, ids)
	if err != nil {
		return 0, err
	}
	var bad int
	for _, id := range conversations {
		rows, err := checkTreeRowsOf(ctx, pool, id)
		if err != nil {
			return 0, err
		}
		byID := make(map[string]checkTreeRow, len(rows))
		for _, row := range rows {
			byID[row.id] = row
		}
		for _, row := range rows {
			if row.hash == nil {
				continue
			}
			if !checkHashMatches(byID, row) {
				bad++
			}
		}
	}
	return bad, nil
}

func checkTreeRowsOf(ctx context.Context, pool *pgxpool.Pool, conversationID string) ([]checkTreeRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, parent_id::text, derived_from_id::text, kind, role, content::text,
		       path_hash, hash_version, depth
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("tree check: read tree rows of %s: %w", conversationID, err)
	}
	defer rows.Close()
	var out []checkTreeRow
	for rows.Next() {
		var row checkTreeRow
		if err := rows.Scan(&row.id, &row.parent, &row.derived, &row.kind, &row.role, &row.content,
			&row.hash, &row.version, &row.depth); err != nil {
			return nil, fmt.Errorf("tree check: scan tree row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// checkHashMatches reports whether a node's stored hash is the hash its position
// implies. byID holds every row of the conversation, so a parent from another
// conversation is simply absent.
func checkHashMatches(byID map[string]checkTreeRow, row checkTreeRow) bool {
	if row.version == nil || *row.version != convtree.HashVersion {
		return false
	}
	var slot convtree.Hash
	if row.parent == nil {
		if row.depth == nil || *row.depth != 0 {
			return false
		}
		// The seed kind is the BACKFILL's rule, not the row's: the backfill seeds a
		// root with RootKindRoot unless the row's kind is a boundary kind (see its
		// root branch), so a root row of any other kind is seeded the same way here
		// or a correctly hashed tree reads as a bad hash.
		kind := convtree.RootKindRoot
		if row.kind != nil && (*row.kind == KindCompactionSummary || *row.kind == KindClear) {
			kind = *row.kind
		}
		var derivedFrom *convtree.Hash
		if row.derived != nil {
			derived, ok := byID[*row.derived]
			if !ok || len(derived.hash) != checkHashLen {
				return false
			}
			h := convtree.Hash(derived.hash)
			derivedFrom = &h
		}
		slot = convtree.RootSeed(kind, derivedFrom)
	} else {
		parent, ok := byID[*row.parent]
		if !ok || len(parent.hash) != checkHashLen {
			return false
		}
		slot = convtree.Hash(parent.hash)
	}
	want, err := convtree.NodeHash(slot, convtree.Msg{Role: row.role, Content: row.content})
	if err != nil {
		return false
	}
	return bytes.Equal(want[:], row.hash)
}

// checkWorkingSets compares the working path of every conversation that still
// carries a LEGACY RESUME HORIZON against the working set that horizon
// described: the rows at and after it, in ordinal order.
//
// The horizon filter is what makes this check mean anything after the cutover.
// Nothing writes resume_from_ordinal any more, so "the working path equals every
// row in ordinal order" is no longer a property of correct data: a conversation
// that compacts or forks now legitimately leaves sibling rows off its head path.
// Only conversations whose storage actually used the horizon — the ones the
// backfill had to reproduce — are still expected to match, so this comparison is
// the cutover's net over pre-tree conversations and nothing else.
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

// checkConversationsWithMessages lists the conversations this check compares:
// the ones with messages AND a legacy resume horizon. A conversation without the
// horizon was never stored that way (or was written after the cutover), so it has
// no legacy working set to reproduce — see checkWorkingSets.
func checkConversationsWithMessages(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT c.id::text FROM conversations.conversation c
		  WHERE ($1::uuid[] IS NULL OR c.id = ANY($1::uuid[]))
		    AND c.resume_from_ordinal IS NOT NULL
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
	return checkCompareMessages(a, b, SameMessage)
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
