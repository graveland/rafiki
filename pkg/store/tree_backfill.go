// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/convtree"
)

// BackfillReport is what one conversation's backfill did. The counts exist so a
// rehearsal over real data can be read rather than guessed at: Nodes is every
// row that got a tree position, InterimCopies the rows recognised as a replay's
// copies of nodes already placed, UnmatchedCopies the copies that matched
// nothing (a copy whose prefix does not line up with the working path is a
// shape the backfill could not explain, and must be zero), and
// OtherDuplicates the rows whose hash already existed for some other reason.
type BackfillReport struct {
	Conversations   int
	Nodes           int
	InterimCopies   int
	UnmatchedCopies int
	OtherDuplicates int
}

// BackfillConversationTree gives every message row of one conversation its tree
// position: parent_id, derived_from_id, path_hash, hash_version, depth and
// provider_message_id, plus the conversation's head_id.
//
// It walks the rows in ordinal order, rebuilding the working path as capture
// built it: a boundary row (kind compaction_summary or clear, or ordinal 0)
// starts a new root, every other row is a child of the cursor — except a
// compaction_tail row outside a real tail, which is a replay's copy of a node
// already placed and stays a legacy row with no tree position.
//
// Nothing is deleted or renumbered, and re-running it over the same rows
// assigns the same values.
func BackfillConversationTree(ctx context.Context, tx pgx.Tx, conversationID string) (BackfillReport, error) {
	report, err := backfillConversationTree(ctx, tx, conversationID)
	if err != nil {
		// The id is the whole diagnosis for a failure over a real conversation:
		// the error alone names a row in a table of hundreds of thousands.
		return report, fmt.Errorf("backfill conversation %s: %w", conversationID, err)
	}
	return report, nil
}

// backfillRow is one legacy message row, as read before the walk.
type backfillRow struct {
	id      string
	ordinal int32
	role    string
	content []byte
	kind    *string
}

// backfillNode is one node of the tree: a row that has a tree position. parent
// indexes the node's parent in the walk's node slice, -1 for a root, so the
// working path can be rebuilt by walking parents without re-deriving a chain of
// copies.
type backfillNode struct {
	id     string
	hash   convtree.Hash
	depth  int32
	msg    convtree.Msg
	parent int32
}

// backfillInterimRun is a run of compaction_tail copies: capture re-stored a
// replayed request prefix from message 0, so the run's j-th copy duplicates the
// j-th node of the path as it stood when the run began.
type backfillInterimRun struct {
	start []int32
	idx   int
	// broken records that a copy in this run matched nothing. The rest of the
	// run is stored as ordinary children: once the prefix diverges, every
	// later copy of it is suspect, and re-testing them would turn one
	// unexplained row into a dozen.
	broken bool
}

// match reports whether the run's next copy duplicates the next node of the
// path it started from, advancing the run when it does.
func (r *backfillInterimRun) match(nodes []backfillNode, m convtree.Msg) bool {
	if r.broken || r.idx >= len(r.start) {
		return false
	}
	if !SameMessage(nodes[r.start[r.idx]].msg, m) {
		return false
	}
	r.idx++
	return true
}

func backfillConversationTree(ctx context.Context, tx pgx.Tx, conversationID string) (BackfillReport, error) {
	var report BackfillReport

	rows, err := backfillReadRows(ctx, tx, conversationID)
	if err != nil {
		return report, err
	}
	if len(rows) == 0 {
		return report, nil
	}
	report.Conversations = 1

	providers, err := backfillReadProviderIDs(ctx, tx, conversationID)
	if err != nil {
		return report, err
	}

	ids := make([]string, 0, len(rows))
	parentIDs := make([]*string, 0, len(rows))
	derivedIDs := make([]*string, 0, len(rows))
	hashes := make([][]byte, 0, len(rows))
	versions := make([]int16, 0, len(rows))
	depths := make([]int32, 0, len(rows))
	providerIDs := make([]*string, 0, len(rows))

	// nodes is every node placed so far; path is the chain from the current
	// root to the cursor, as indices into it. byHash finds the node a repeated
	// hash already placed, so the cursor can move onto it.
	var (
		nodes  []backfillNode
		path   []int32
		byHash = map[convtree.Hash]int32{}
	)
	var run *backfillInterimRun
	inRealTail := false

	for _, row := range rows {
		msg := convtree.Msg{Role: row.role, Content: row.content}
		boundary := row.kind != nil && (*row.kind == KindCompactionSummary || *row.kind == KindClear)
		tail := row.kind != nil && *row.kind == KindCompactionTail
		// A row with no cursor to attach to can only be a root; in real data
		// that is the same thing as ordinal 0.
		isRoot := len(path) == 0 || row.ordinal == 0 || boundary

		switch {
		case isRoot:
			run = nil
			inRealTail = boundary && *row.kind == KindCompactionSummary
		case tail && inRealTail:
			// Fundi's real tail: copies the summary did not cover, appended as
			// ordinary children of the summary.
		case tail:
			if run == nil {
				run = &backfillInterimRun{start: append([]int32(nil), path...)}
			}
			if run.match(nodes, msg) {
				report.InterimCopies++
				// A copy of the run's prefix, never a slice of it: the path is
				// appended to as soon as the run ends, and an alias would write
				// through into the run's own start.
				path = append([]int32(nil), run.start[:run.idx]...)
				continue
			}
			slog.Warn("backfill: interim copy matched nothing",
				"conversation", conversationID, "ordinal", row.ordinal, "copy", run.idx)
			report.UnmatchedCopies++
			run.broken = true
		default:
			run = nil
			inRealTail = false
		}

		var (
			parentID  *string
			derivedID *string
			slot      convtree.Hash
			depth     int32
			parent    int32 = -1
		)
		if isRoot {
			kind := convtree.RootKindRoot
			if boundary {
				kind = *row.kind
			}
			// The seed is derived from the node a root was cut from, which for a
			// claude compaction can be the summarization side branch rather than
			// the head the client named: derived_from_id records that lineage,
			// the hash deliberately does not (see convtree.RootSeed).
			var derivedFrom *convtree.Hash
			if len(path) > 0 {
				cursor := nodes[path[len(path)-1]]
				id, cursorHash := cursor.id, cursor.hash
				derivedID = &id
				derivedFrom = &cursorHash
			}
			slot = convtree.RootSeed(kind, derivedFrom)
		} else {
			parent = path[len(path)-1]
			cursor := nodes[parent]
			id := cursor.id
			parentID = &id
			slot = cursor.hash
			depth = cursor.depth + 1
		}

		hash, err := convtree.NodeHash(slot, msg)
		if err != nil {
			return report, fmt.Errorf("hash ordinal %d: %w", row.ordinal, err)
		}
		if existing, ok := byHash[hash]; ok {
			report.OtherDuplicates++
			path = backfillPathTo(nodes, existing)
			// A duplicate root re-states the root it matched, so it opens the
			// same kind of boundary: a repeated summary is still followed by a
			// real tail.
			inRealTail = isRoot && boundary && *row.kind == KindCompactionSummary
			continue
		}

		nodes = append(nodes, backfillNode{id: row.id, hash: hash, depth: depth, msg: msg, parent: parent})
		index := int32(len(nodes) - 1)
		if isRoot {
			// A root is a chain of its own: the cursor restarts at it, or the
			// next row would be appended to a path from before the boundary.
			path = []int32{index}
		} else {
			path = append(path, index)
		}
		byHash[hash] = index
		report.Nodes++

		var providerID *string
		if row.role == "assistant" {
			if pid, ok := providers[row.ordinal]; ok {
				providerID = &pid
			}
		}

		ids = append(ids, row.id)
		parentIDs = append(parentIDs, parentID)
		derivedIDs = append(derivedIDs, derivedID)
		hashes = append(hashes, append([]byte(nil), hash[:]...))
		versions = append(versions, convtree.HashVersion)
		depths = append(depths, depth)
		providerIDs = append(providerIDs, providerID)
	}

	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, backfillUpdateSQL,
			ids, parentIDs, derivedIDs, hashes, versions, depths, providerIDs); err != nil {
			return report, fmt.Errorf("write tree positions: %w", err)
		}
	}
	if len(path) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE conversations.conversation SET head_id = $2 WHERE id = $1::uuid`,
			conversationID, nodes[path[len(path)-1]].id); err != nil {
			return report, fmt.Errorf("write head: %w", err)
		}
	}
	return report, nil
}

// SameMessage reports whether two stored messages are the same message.
//
// It is the one identity the backfill, the tree check and live capture share:
// capture compares a request's replayed message with the node a
// previous_message_id names, the backfill compares a re-stored copy with the
// node it duplicates, and the check compares a legacy working set with the path
// that replaced it. One rule, one implementation — two would drift.
//
// Canonical equality alone is not enough on real data. Capture stored an
// assistant reply as the SDK's marshalled message, so one side of a real pair
// carries a field for every variant of the block union — "text":"",
// "id":"", "name":"", "signature":"", "data":"", "tool_use_id":"",
// "content":{"type":"web_search_tool_result_error","error_code":""} — while the
// other side is the provider's own JSON and may carry provider fields the SDK
// models but the raw form omits, such as "caller":{"type":"direct"}. Round-
// tripping both through the SDK does not remove that asymmetry, so the second
// pass compares the two decoded values by projection instead: a field present on
// only one side is ignored when its value cannot carry meaning, and every other
// difference is a difference.
//
// The rule's only leniency is a ZERO-VALUED field that one side omits. A
// one-sided field whose value is not zero — a tool result's content, an
// is_error, an image, a citation — is a difference, never noise, so a rewritten
// message stays distinct.
func SameMessage(node, copy convtree.Msg) bool {
	if convtree.Equal(node, copy) {
		return true
	}
	if node.Role != copy.Role {
		return false
	}
	a, ok := backfillDecodeMessage(node.Content)
	if !ok {
		return false
	}
	b, ok := backfillDecodeMessage(copy.Content)
	if !ok {
		return false
	}
	return backfillSameValue(a, b)
}

// backfillDecodeMessage decodes stored content the way Canon does: numbers stay
// exact, and a bare JSON string is the shorthand for one text block. Content
// that is not one JSON value is not a message and compares equal to nothing.
func backfillDecodeMessage(content []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	if text, ok := v.(string); ok {
		v = []any{map[string]any{"type": "text", "text": text}}
	}
	return v, true
}

// backfillSameValue reports whether two decoded JSON values are the same
// message: object keys are compared pairwise, a key only one side has is
// tolerated when its value is zero (backfillZeroValue), cache_control is dropped
// exactly as Canon drops it, and lists must agree element for element.
func backfillSameValue(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for key, aValue := range av {
			if key == "cache_control" {
				continue
			}
			bValue, both := bv[key]
			if !both {
				if !backfillZeroValue(aValue) {
					return false
				}
				continue
			}
			if !backfillSameValue(aValue, bValue) {
				return false
			}
		}
		for key, bValue := range bv {
			if key == "cache_control" {
				continue
			}
			if _, both := av[key]; both {
				continue
			}
			if !backfillZeroValue(bValue) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !backfillSameValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// backfillZeroValue reports whether a value cannot carry meaning: null, false,
// "", zero, a list whose elements are all such values, or an object whose every
// value other than its type tag is such a value. That last clause is what makes
// {"type":"direct"} — a provider field marking a directly-called tool — noise
// rather than a difference, and it holds for ANY such tag, not for "direct"
// alone: an object carrying nothing but a type tag has no payload, so the tag by
// itself names nothing two messages can disagree about. A one-sided tag WITH a
// payload is a difference, like any other non-zero one-sided field.
func backfillZeroValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case json.Number:
		n, err := t.Float64()
		return err == nil && n == 0
	case []any:
		for _, element := range t {
			if !backfillZeroValue(element) {
				return false
			}
		}
		return true
	case map[string]any:
		for key, element := range t {
			if key == "type" {
				continue
			}
			if !backfillZeroValue(element) {
				return false
			}
		}
		return true
	}
	return false
}

// backfillPathTo rebuilds the chain from the root to nodes[idx] by walking
// parents, which is what moving the cursor onto an already-placed node means.
func backfillPathTo(nodes []backfillNode, idx int32) []int32 {
	var rev []int32
	for i := idx; i >= 0; i = nodes[i].parent {
		rev = append(rev, i)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// backfillUpdateSQL writes every assignment of one conversation in a single
// statement. The interim copies are absent from the arrays, which is what
// leaves them legacy rows.
const backfillUpdateSQL = `
UPDATE conversations.conversation_message m SET
  parent_id = u.parent_id, derived_from_id = u.derived_from_id, path_hash = u.path_hash,
  hash_version = u.hash_version, depth = u.depth, provider_message_id = u.provider_message_id
FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::bytea[], $5::smallint[], $6::int[], $7::text[])
  AS u(id, parent_id, derived_from_id, path_hash, hash_version, depth, provider_message_id)
WHERE m.id = u.id`

// backfillReadRows reads the conversation's rows in ordinal order, content as
// the stored jsonb text — the exact bytes that get hashed.
func backfillReadRows(ctx context.Context, tx pgx.Tx, conversationID string) ([]backfillRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, ordinal, role, content::text, kind
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer rows.Close()
	var out []backfillRow
	for rows.Next() {
		var row backfillRow
		if err := rows.Scan(&row.id, &row.ordinal, &row.role, &row.content, &row.kind); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// backfillReadProviderIDs maps an ordinal to the provider message id of the
// reply stored there. Later turns win: a reply re-stored under a later turn is
// the one the client was actually sent.
func backfillReadProviderIDs(ctx context.Context, tx pgx.Tx, conversationID string) (map[int32]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT response_ordinal, response_message_id
		  FROM conversations.conversation_turn
		 WHERE conversation_id = $1::uuid AND response_ordinal IS NOT NULL AND response_message_id IS NOT NULL
		 ORDER BY created_at`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read turn replies: %w", err)
	}
	defer rows.Close()
	out := map[int32]string{}
	for rows.Next() {
		var (
			ordinal int32
			id      string
		)
		if err := rows.Scan(&ordinal, &id); err != nil {
			return nil, fmt.Errorf("scan turn reply: %w", err)
		}
		out[ordinal] = id
	}
	return out, rows.Err()
}

// backfillAllConversations is the Go half of migration 0056: it places every
// message row written before the tree existed, then seeds the ordinal
// allocator. It runs inside 0056's transaction, which commits before 0058 adds
// the denormalised counters — 0058 seeds those from the rows this places, so
// writing them here would name columns that do not exist yet.
func backfillAllConversations(ctx context.Context, tx pgx.Tx) error {
	ids, err := backfillConversationsWithLegacyRows(ctx, tx)
	if err != nil {
		return err
	}
	var total BackfillReport
	for _, id := range ids {
		report, err := BackfillConversationTree(ctx, tx, id)
		if err != nil {
			return err
		}
		total.Conversations += report.Conversations
		total.Nodes += report.Nodes
		total.InterimCopies += report.InterimCopies
		total.UnmatchedCopies += report.UnmatchedCopies
		total.OtherDuplicates += report.OtherDuplicates
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.conversation SET next_ordinal = coalesce((
			SELECT max(ordinal)+1 FROM conversations.conversation_message
			 WHERE conversation_id = conversation.id), 0)`); err != nil {
		return fmt.Errorf("seed next_ordinal: %w", err)
	}
	slog.Info("conversation tree backfill",
		"conversations", total.Conversations, "nodes", total.Nodes,
		"interim_copies", total.InterimCopies, "unmatched_copies", total.UnmatchedCopies,
		"other_duplicates", total.OtherDuplicates)
	return nil
}

// backfillConversationsWithLegacyRows lists the conversations that still have
// rows without a tree position. The rows are drained before any per-conversation
// query runs: a pgx connection cannot serve a second query while one is open.
func backfillConversationsWithLegacyRows(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT conversation_id::text FROM conversations.conversation_message
		 WHERE path_hash IS NULL ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list conversations to backfill: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan conversation id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
