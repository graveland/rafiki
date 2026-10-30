// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// ConvBackend is conv.Backend over Postgres: one conversation's node tree in
// conversations.conversation_message, its position on
// conversations.conversation.
//
// It writes exactly what AppendChain writes, in the same transaction shape —
// the conversation row locked FOR UPDATE, the lease re-checked under FOR SHARE,
// the nodes inserted with ON CONFLICT (conversation_id, path_hash) DO NOTHING,
// and the conversation's counters moved in one UPDATE — so the two write paths
// cannot disagree about a conversation's tree.
type ConvBackend struct{ pool *pgxpool.Pool }

// NewConvBackend returns a conv.Backend backed by pool.
func NewConvBackend(pool *pgxpool.Pool) *ConvBackend { return &ConvBackend{pool: pool} }

var _ conv.Backend = (*ConvBackend)(nil)

// refColumns is the light column list Walk reads: a node's place in the tree
// without its content.
const refColumns = `
	id::text, coalesce(parent_id::text, ''), coalesce(derived_from_id::text, ''),
	ordinal, coalesce(depth, 0), coalesce(kind, ''), created_at, path_hash`

// nodeColumns is refColumns plus the stored message: what Fetch, Roots and
// ByProviderMessageID hand back.
const nodeColumns = refColumns + `,
	role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''),
	input_tokens, output_tokens, coalesce(provider_message_id, '')`

// rowScanner is the half of pgx.Row and pgx.Rows a scan helper needs.
type rowScanner interface {
	Scan(dest ...any) error
}

// State returns the conversation's head, next ordinal and pending-clear flag.
func (b *ConvBackend) State(ctx context.Context, convID string) (conv.State, error) {
	var st conv.State
	err := b.pool.QueryRow(ctx, `
		SELECT coalesce(head_id::text, ''), next_ordinal, clear_pending
		  FROM conversations.conversation WHERE id = $1::uuid`, convID).
		Scan(&st.HeadID, &st.NextOrdinal, &st.ClearPending)
	if errors.Is(err, pgx.ErrNoRows) {
		return conv.State{}, fmt.Errorf("%w: %s", conv.ErrNotFound, convID)
	}
	if err != nil {
		return conv.State{}, fmt.Errorf("conv backend: state %s: %w", convID, err)
	}
	return st, nil
}

// Walk returns the chain from fromID toward the root, head-first, in one
// recursive CTE. Every step is scoped to the conversation, so a parent id from
// another conversation cannot walk into it; a root's DerivedFromID is followed
// only when crossRoots is set, which is what stops the working path at a
// compaction or clear boundary.
func (b *ConvBackend) Walk(ctx context.Context, convID, fromID string, crossRoots bool) ([]conv.Ref, error) {
	if fromID == "" {
		return nil, nil
	}
	rows, err := b.pool.Query(ctx, `
		WITH RECURSIVE walk AS (
		  SELECT m.id, m.parent_id, m.derived_from_id, m.ordinal, coalesce(m.depth, 0) AS depth,
		         coalesce(m.kind, '') AS kind, m.created_at, m.path_hash, 0 AS step
		    FROM conversations.conversation_message m
		   WHERE m.conversation_id = $1::uuid AND m.id = $2::uuid
		  UNION ALL
		  SELECT p.id, p.parent_id, p.derived_from_id, p.ordinal, coalesce(p.depth, 0),
		         coalesce(p.kind, ''), p.created_at, p.path_hash, walk.step + 1
		    FROM conversations.conversation_message p
		    JOIN walk ON p.id = CASE
		                         WHEN walk.parent_id IS NOT NULL THEN walk.parent_id
		                         WHEN $3::boolean AND walk.derived_from_id IS NOT NULL
		                           THEN walk.derived_from_id
		                       END
		   WHERE p.conversation_id = $1::uuid
		)
		SELECT `+refColumns+`
		  FROM walk ORDER BY step`, convID, fromID, crossRoots)
	if err != nil {
		return nil, fmt.Errorf("conv backend: walk %s from %s: %w", convID, fromID, err)
	}
	defer rows.Close()

	var out []conv.Ref
	for rows.Next() {
		ref, err := scanRef(convID, rows)
		if err != nil {
			return nil, fmt.Errorf("conv backend: walk %s: %w", convID, err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conv backend: walk %s: %w", convID, err)
	}
	return out, nil
}

// Fetch returns the nodes of ids the conversation holds, in the order asked
// for. An id it does not hold is omitted, as an IN query would.
func (b *ConvBackend) Fetch(ctx context.Context, convID string, ids []string) ([]*conv.Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := b.pool.Query(ctx, `
		SELECT `+nodeColumns+`
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND id = ANY($2::uuid[])`, convID, ids)
	if err != nil {
		return nil, fmt.Errorf("conv backend: fetch %s: %w", convID, err)
	}
	defer rows.Close()

	byID := make(map[string]*conv.Node, len(ids))
	for rows.Next() {
		n, err := scanNode(convID, rows)
		if err != nil {
			return nil, fmt.Errorf("conv backend: fetch %s: %w", convID, err)
		}
		byID[n.ID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conv backend: fetch %s: %w", convID, err)
	}
	out := make([]*conv.Node, 0, len(byID))
	for _, id := range ids {
		if n, ok := byID[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Lookup returns the refs of the nodes the conversation holds under hashes.
func (b *ConvBackend) Lookup(ctx context.Context, convID string, hashes []convtree.Hash) (map[convtree.Hash]conv.Ref, error) {
	out := make(map[convtree.Hash]conv.Ref, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	raw := make([][]byte, len(hashes))
	for i, h := range hashes {
		raw[i] = h[:]
	}
	rows, err := b.pool.Query(ctx, `
		SELECT `+refColumns+`
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash = ANY($2::bytea[])`, convID, raw)
	if err != nil {
		return nil, fmt.Errorf("conv backend: lookup %s: %w", convID, err)
	}
	defer rows.Close()

	for rows.Next() {
		ref, err := scanRef(convID, rows)
		if err != nil {
			return nil, fmt.Errorf("conv backend: lookup %s: %w", convID, err)
		}
		out[ref.PathHash] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conv backend: lookup %s: %w", convID, err)
	}
	return out, nil
}

// Roots returns the conversation's parent-less placed nodes, newest ordinal
// first.
func (b *ConvBackend) Roots(ctx context.Context, convID string) ([]*conv.Node, error) {
	return b.queryNodes(ctx, convID, `
		SELECT `+nodeColumns+`
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND parent_id IS NULL AND path_hash IS NOT NULL
		 ORDER BY ordinal DESC`, convID)
}

// ByProviderMessageID returns the newest placed node carrying the upstream
// message id.
func (b *ConvBackend) ByProviderMessageID(ctx context.Context, convID, providerMessageID string) (*conv.Node, bool, error) {
	nodes, err := b.queryNodes(ctx, convID, `
		SELECT `+nodeColumns+`
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND provider_message_id = $2 AND path_hash IS NOT NULL
		 ORDER BY ordinal DESC LIMIT 1`, convID, providerMessageID)
	if err != nil {
		return nil, false, err
	}
	if len(nodes) == 0 {
		return nil, false, nil
	}
	return nodes[0], true, nil
}

// HasChild reports whether parentID has a child in the conversation. It reads
// parent_id only, which is the edge AppendChain's fork check counts.
func (b *ConvBackend) HasChild(ctx context.Context, convID, parentID string) (bool, error) {
	var exists bool
	err := b.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM conversations.conversation_message
		               WHERE conversation_id = $1::uuid AND parent_id = $2::uuid)`,
		convID, parentID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("conv backend: has child %s of %s: %w", parentID, convID, err)
	}
	return exists, nil
}

// Commit applies one atomic tree write, mirroring AppendChain statement for
// statement: the conversation row is locked FOR UPDATE (which is what makes the
// head comparison meaningful and the head move atomic with the insert), a held
// lease is re-checked under FOR SHARE (a takeover's UPDATE blocks until this
// transaction ends, so a writer that wakes after a takeover writes nothing), the
// nodes are inserted with the same ON CONFLICT (conversation_id, path_hash) DO
// NOTHING, and the conversation's counters move in one UPDATE.
//
// A head or next_ordinal mismatch is conv.ErrConflict, an insert that returns
// no row is conv.ErrConflict (the path hash is already stored), and a fence
// failure is conv.ErrLeaseLost. The created_at it returns is the transaction's,
// shared by every row inserted here; a commit with no rows returns the zero
// time, since nothing was stamped.
func (b *ConvBackend) Commit(ctx context.Context, c conv.Commit) (time.Time, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("conv commit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		head        string
		nextOrdinal int
	)
	err = tx.QueryRow(ctx, `
		SELECT coalesce(head_id::text, ''), next_ordinal
		  FROM conversations.conversation WHERE id = $1::uuid FOR UPDATE`, c.ConversationID).
		Scan(&head, &nextOrdinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: %s", conv.ErrNotFound, c.ConversationID)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("conv commit: lock conversation: %w", err)
	}
	if head != c.ExpectHead || nextOrdinal != c.ExpectNextOrdinal {
		return time.Time{}, fmt.Errorf("%w: conversation %s is at head %q ordinal %d, not head %q ordinal %d",
			conv.ErrConflict, c.ConversationID, head, nextOrdinal, c.ExpectHead, c.ExpectNextOrdinal)
	}

	if err := fenceLease(ctx, tx, c.ConversationID, c.Lease); err != nil {
		return time.Time{}, fmt.Errorf("conv commit: %w", err)
	}

	inserted := make([]string, 0, len(c.Rows))
	var created time.Time
	for i, row := range c.Rows {
		var at time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO conversations.conversation_message
				(id, conversation_id, ordinal, role, content, tool_use_ids, input_tokens,
				 output_tokens, stop_reason, kind, parent_id, derived_from_id, path_hash,
				 hash_version, depth, provider_message_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11::uuid, $12::uuid,
			        $13, $14, $15, $16)
			ON CONFLICT (conversation_id, path_hash) DO NOTHING
			RETURNING created_at`,
			row.ID, c.ConversationID, row.Ordinal, row.Msg.Role.String(), row.Content,
			row.ToolUseIDs, row.InputTokens, row.OutputTokens, nullString(row.StopReason),
			nullString(row.Kind), nullString(row.ParentID), nullString(row.DerivedFromID),
			row.PathHash[:], convtree.HashVersion, row.Depth, nullString(row.ProviderMessageID),
		).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, fmt.Errorf("%w: node %d's path hash is already stored", conv.ErrConflict, i)
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("conv commit: insert node %d: %w", i, err)
		}
		inserted = append(inserted, row.ID)
		if i == 0 {
			created = at
		}
	}

	var newHead any
	if c.NewHead != "" {
		newHead = c.NewHead
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.conversation SET
			next_ordinal   = next_ordinal + $2,
			head_id        = $3::uuid,
			node_count     = node_count + $2,
			fork_count     = fork_count + $4,
			fallback_count = fallback_count + $5,
			payload_bytes  = payload_bytes + coalesce((
				SELECT sum(octet_length(content::text)) FROM conversations.conversation_message
				 WHERE id = ANY($6::uuid[])), 0),
			clear_pending  = CASE WHEN $7 THEN false ELSE clear_pending END
		 WHERE id = $1::uuid`,
		c.ConversationID, len(inserted), newHead, c.Forks, boolInt(c.Fallback),
		inserted, c.ConsumeClearPending); err != nil {
		return time.Time{}, fmt.Errorf("conv commit: update conversation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, fmt.Errorf("conv commit: commit: %w", err)
	}
	return created, nil
}

// queryNodes runs a node-columns query and scans every row.
func (b *ConvBackend) queryNodes(ctx context.Context, convID, sql string, args ...any) ([]*conv.Node, error) {
	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("conv backend: read %s: %w", convID, err)
	}
	defer rows.Close()

	var out []*conv.Node
	for rows.Next() {
		n, err := scanNode(convID, rows)
		if err != nil {
			return nil, fmt.Errorf("conv backend: read %s: %w", convID, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conv backend: read %s: %w", convID, err)
	}
	return out, nil
}

// scanRef reads the light columns of one row. A legacy row written before the
// tree existed has no path hash; its ref carries the zero hash, which no
// Lookup can match and no append can reuse.
func scanRef(convID string, r rowScanner) (conv.Ref, error) {
	var (
		ref  conv.Ref
		hash []byte
	)
	if err := r.Scan(&ref.ID, &ref.ParentID, &ref.DerivedFromID, &ref.Ordinal, &ref.Depth,
		&ref.Kind, &ref.CreatedAt, &hash); err != nil {
		return conv.Ref{}, err
	}
	ref.ConversationID = convID
	h, err := decodeHash(ref.ID, hash)
	if err != nil {
		return conv.Ref{}, err
	}
	ref.PathHash = h
	return ref, nil
}

// scanNode reads a node row: a ref plus the stored message. Content is the
// message re-encoded, not the column's bytes, because jsonb does not preserve
// the text it was given — it reorders keys and normalises whitespace. The
// re-encode is what a fresh append of the same message stores, so a node read
// back compares equal to the node that was appended; its identity is its path
// hash either way.
func scanNode(convID string, r rowScanner) (*conv.Node, error) {
	var (
		n       conv.Node
		hash    []byte
		role    string
		content []byte
	)
	if err := r.Scan(&n.ID, &n.ParentID, &n.DerivedFromID, &n.Ordinal, &n.Depth, &n.Kind,
		&n.CreatedAt, &hash, &role, &content, &n.ToolUseIDs, &n.StopReason,
		&n.InputTokens, &n.OutputTokens, &n.ProviderMessageID); err != nil {
		return nil, err
	}
	n.ConversationID = convID
	h, err := decodeHash(n.ID, hash)
	if err != nil {
		return nil, err
	}
	n.PathHash = h
	message, err := msganthropic.Decode(role, content)
	if err != nil {
		return nil, fmt.Errorf("decode node %s: %w", n.ID, err)
	}
	n.Msg = message
	stored, err := msganthropic.EncodeContent(message)
	if err != nil {
		return nil, fmt.Errorf("re-encode node %s: %w", n.ID, err)
	}
	n.Content = stored
	return &n, nil
}

// decodeHash turns a stored path hash into a convtree.Hash.
func decodeHash(id string, raw []byte) (convtree.Hash, error) {
	var h convtree.Hash
	if raw == nil {
		return h, nil
	}
	if len(raw) != len(h) {
		return h, fmt.Errorf("node %s has a malformed path hash", id)
	}
	copy(h[:], raw)
	return h, nil
}
