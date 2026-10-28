// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"
)

// ChainNode is one message to append. Content is the stored JSON (already
// jsonbSafe).
type ChainNode struct {
	Role              string
	Content           []byte
	ToolUseIDs        []string
	InputTokens       *int64
	OutputTokens      *int64
	StopReason        string  // "" stores NULL
	Kind              *string // non-root rows only, e.g. KindCompactionTail; nil for ordinary rows
	ProviderMessageID string  // "" stores NULL
}

// ChainAppend describes one append onto the conversation tree: where the chain
// starts and the nodes to append under it. Exactly one parent mode is set.
type ChainAppend struct {
	ConversationID string
	// Parent: exactly one of ParentID, ParentFromHead, or Root.
	ParentID       string
	ParentFromHead bool   // parent = conversation.head_id read under the lock; NULL head makes Nodes[0] a RootKindRoot root
	Root           string // non-empty: Nodes[0] is a root of this kind (convtree.RootKind*)
	// Root only: exactly one of DerivedFromID or DerivedFromHead (or neither, for RootKindRoot).
	DerivedFromID       string
	DerivedFromHead     bool
	Nodes               []ChainNode // may be empty only with ParentID: then the call just moves head_id to ParentID
	Fallback            bool        // add 1 to conversation.fallback_count
	ConsumeClearPending bool        // also set conversation.clear_pending = false, in the same transaction
}

// ChainResult is what an append did: the identity of every input node, in
// order, and how many of them this call actually inserted.
type ChainResult struct {
	Nodes    []Appended // one per input node, in order, inserted or existing
	Inserted int        // how many rows this call inserted
}

// chainParent is the resolved parent slot a chain hangs from.
type chainParent struct {
	// id is the parent ROW, empty for a root.
	id string
	// slot is the hash the first node chains from: the parent's path hash, or
	// a RootSeed for a root.
	slot convtree.Hash
	// depth is the depth Nodes[0] gets.
	depth int
}

// chainExisting is one already-stored node of the chain being appended.
type chainExisting struct {
	id        string
	ordinal   int
	depth     int
	createdAt time.Time
}

// AppendChain appends a chain of messages to a conversation, inserting only the
// suffix of the chain the conversation does not already hold.
//
// The conversation row is locked FOR UPDATE for the whole transaction, which is
// what allocates a gap-free ordinal range and what makes the head move atomic
// with the insert. A held lease is re-checked inside the same transaction with
// FOR SHARE on the lease row: a takeover's UPDATE blocks until this transaction
// ends, so a stalled writer that wakes after takeover still writes nothing.
//
// Existing nodes of a chain must form a PREFIX of it. A gap means the tree
// disagrees with the chain being appended, which is reported rather than
// repaired — silently rewriting history is the failure this whole scheme exists
// to prevent.
func AppendChain(ctx context.Context, pool *pgxpool.Pool, lease Lease, req ChainAppend) (ChainResult, error) {
	if err := req.validate(); err != nil {
		return ChainResult{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return ChainResult{}, fmt.Errorf("append chain: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		head        string
		nextOrdinal int
	)
	err = tx.QueryRow(ctx, `
		SELECT coalesce(head_id::text, ''), next_ordinal
		  FROM conversations.conversation WHERE id = $1::uuid FOR UPDATE`, req.ConversationID).Scan(&head, &nextOrdinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChainResult{}, fmt.Errorf("append chain: conversation %s not found", req.ConversationID)
	}
	if err != nil {
		return ChainResult{}, fmt.Errorf("append chain: lock conversation: %w", err)
	}

	if err := fenceLease(ctx, tx, req.ConversationID, lease); err != nil {
		return ChainResult{}, fmt.Errorf("append chain: %w", err)
	}

	parent, isRoot, rootKind, derivedFromID, err := resolveChainParent(ctx, tx, req, head)
	if err != nil {
		return ChainResult{}, fmt.Errorf("append chain: %w", err)
	}

	msgs := make([]convtree.Msg, len(req.Nodes))
	for i, n := range req.Nodes {
		msgs[i] = convtree.Msg{Role: n.Role, Content: n.Content}
	}
	hashes, err := convtree.Chain(parent.slot, msgs)
	if err != nil {
		return ChainResult{}, fmt.Errorf("append chain: hash chain: %w", err)
	}

	existing, err := loadChainNodes(ctx, tx, req.ConversationID, hashes)
	if err != nil {
		return ChainResult{}, fmt.Errorf("append chain: %w", err)
	}

	// m is the first node the conversation does not already hold; everything
	// before it must be present and everything from it on must be absent.
	m := len(hashes)
	for i, h := range hashes {
		if _, ok := existing[h]; !ok {
			m = i
			break
		}
	}
	for i := m; i < len(hashes); i++ {
		if _, ok := existing[hashes[i]]; ok {
			return ChainResult{}, fmt.Errorf("append chain: tree invariant broken: node %d exists but node %d does not", i, m)
		}
	}

	forks := 0
	if m < len(hashes) {
		forkParent := parent.id
		if m > 0 {
			forkParent = existing[hashes[m-1]].id
		}
		if forkParent != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM conversations.conversation_message WHERE parent_id = $1::uuid)`,
				forkParent).Scan(&exists); err != nil {
				return ChainResult{}, fmt.Errorf("append chain: fork check: %w", err)
			}
			if exists {
				forks = 1
			}
		}
	}

	result := ChainResult{Nodes: make([]Appended, len(hashes))}
	ids := make([]string, len(hashes))
	for i, h := range hashes {
		if e, ok := existing[h]; ok {
			ids[i] = e.id
			result.Nodes[i] = Appended{ID: e.id, CreatedAt: e.createdAt, Ordinal: e.ordinal, Depth: e.depth}
		}
	}
	insertedIDs := make([]string, 0, len(hashes)-m)
	for i := m; i < len(hashes); i++ {
		id := uuid.Must(uuid.NewV7()).String()
		ids[i] = id
		insertedIDs = append(insertedIDs, id)

		nodeParent := parent.id
		if i > 0 {
			nodeParent = ids[i-1]
		}
		var derived any
		if isRoot && i == 0 && derivedFromID != "" {
			derived = derivedFromID
		}
		var nodeKind any
		if isRoot && i == 0 {
			if rootKind == convtree.RootKindCompaction || rootKind == convtree.RootKindClear {
				nodeKind = rootKind
			}
		} else {
			nodeKind = req.Nodes[i].Kind
		}
		ordinal := nextOrdinal + (i - m)
		depth := parent.depth + i

		var createdAt time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO conversations.conversation_message
				(id, conversation_id, ordinal, role, content, tool_use_ids, input_tokens,
				 output_tokens, stop_reason, kind, parent_id, derived_from_id, path_hash,
				 hash_version, depth, provider_message_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11::uuid, $12::uuid,
			        $13, $14, $15, $16)
			ON CONFLICT (conversation_id, path_hash) DO NOTHING
			RETURNING created_at`,
			id, req.ConversationID, ordinal, req.Nodes[i].Role, req.Nodes[i].Content,
			req.Nodes[i].ToolUseIDs, req.Nodes[i].InputTokens, req.Nodes[i].OutputTokens,
			nullString(req.Nodes[i].StopReason), nodeKind, nullString(nodeParent), derived,
			hashes[i][:], convtree.HashVersion, depth, nullString(req.Nodes[i].ProviderMessageID),
		).Scan(&createdAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ChainResult{}, fmt.Errorf("append chain: insert of node %d conflicted", i)
		}
		if err != nil {
			return ChainResult{}, fmt.Errorf("append chain: insert node %d: %w", i, err)
		}
		result.Nodes[i] = Appended{ID: id, CreatedAt: createdAt, Ordinal: ordinal, Depth: depth}
	}
	result.Inserted = len(insertedIDs)

	headID := req.ParentID
	if len(ids) > 0 {
		headID = ids[len(ids)-1]
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
		req.ConversationID, result.Inserted, headID, forks, boolInt(req.Fallback),
		insertedIDs, req.ConsumeClearPending); err != nil {
		return ChainResult{}, fmt.Errorf("append chain: update conversation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ChainResult{}, fmt.Errorf("append chain: commit: %w", err)
	}
	return result, nil
}

// AppendChain appends a chain under m's lease.
func (m *Messages) AppendChain(ctx context.Context, req ChainAppend) (ChainResult, error) {
	return AppendChain(ctx, m.pool, m.lease, req)
}

// LoadPath returns the conversation's working path: the chain of nodes from
// head_id up parent_id to a root, in depth order. A conversation with no head
// returns an empty slice.
func (m *Messages) LoadPath(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := m.pool.Query(ctx, `
		WITH RECURSIVE path AS (
		  SELECT m.* FROM conversations.conversation_message m
		    JOIN conversations.conversation c ON c.head_id = m.id
		   WHERE c.id = $1::uuid
		  UNION ALL
		  SELECT p.* FROM conversations.conversation_message p JOIN path ON p.id = path.parent_id
		)
		SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''), kind, input_tokens, created_at, id::text
		  FROM path ORDER BY depth`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("load path: %w", err)
	}
	return scanMessages(rows)
}

// RetractHead moves the conversation's head back to the parent of its current
// head, which must be a user row. It is a head move, never a delete: the
// retracted row survives, unreachable from the working path, so a retry can
// append a different node at the same position.
func (m *Messages) RetractHead(ctx context.Context, conversationID, id string) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("retract head: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var head string
	err = tx.QueryRow(ctx, `
		SELECT coalesce(head_id::text, '') FROM conversations.conversation
		 WHERE id = $1::uuid FOR UPDATE`, conversationID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("retract head: conversation %s not found", conversationID)
	}
	if err != nil {
		return fmt.Errorf("retract head: lock conversation: %w", err)
	}

	if err := fenceLease(ctx, tx, conversationID, m.lease); err != nil {
		return fmt.Errorf("retract head: %w", err)
	}

	if head != id {
		return fmt.Errorf("retract head: %s is not the head user row", id)
	}
	var (
		role   string
		parent *string
	)
	err = tx.QueryRow(ctx, `
		SELECT role, parent_id::text FROM conversations.conversation_message
		 WHERE id = $1::uuid AND conversation_id = $2::uuid`, id, conversationID).Scan(&role, &parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("retract head: %s is not the head user row", id)
	}
	if err != nil {
		return fmt.Errorf("retract head: read head: %w", err)
	}
	if role != "user" {
		return fmt.Errorf("retract head: %s is not the head user row", id)
	}

	var newHead any
	if parent != nil {
		newHead = *parent
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.conversation SET head_id = $2::uuid WHERE id = $1::uuid`,
		conversationID, newHead); err != nil {
		return fmt.Errorf("retract head: update head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("retract head: commit: %w", err)
	}
	return nil
}

// validate refuses a request that does not describe exactly one well-formed
// append, before any transaction is opened.
func (r ChainAppend) validate() error {
	modes := 0
	if r.ParentID != "" {
		modes++
	}
	if r.ParentFromHead {
		modes++
	}
	if r.Root != "" {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("append chain: exactly one of ParentID, ParentFromHead or Root is required")
	}
	if len(r.Nodes) == 0 && r.ParentID == "" {
		return fmt.Errorf("append chain: no nodes to append")
	}
	switch r.Root {
	case "", convtree.RootKindRoot, convtree.RootKindCompaction, convtree.RootKindClear:
	default:
		return fmt.Errorf("append chain: unknown root kind %q", r.Root)
	}
	if r.Root == "" && (r.DerivedFromID != "" || r.DerivedFromHead) {
		return fmt.Errorf("append chain: derived-from requires a root")
	}
	if r.DerivedFromID != "" && r.DerivedFromHead {
		return fmt.Errorf("append chain: derived-from is DerivedFromID or DerivedFromHead, not both")
	}
	return nil
}

// resolveChainParent finds the slot the chain starts from. isRoot reports
// whether Nodes[0] is a root (an explicit Root, or a ParentFromHead append on a
// conversation with no head yet), rootKind the resolved root kind, and
// derivedFromID the node a root is derived from ("" when none).
func resolveChainParent(ctx context.Context, tx pgx.Tx, req ChainAppend, head string) (parent chainParent, isRoot bool, rootKind, derivedFromID string, err error) {
	if req.ParentID != "" || (req.ParentFromHead && head != "") {
		parentID := req.ParentID
		if parentID == "" {
			parentID = head
		}
		hash, depth, err := readTreeParent(ctx, tx, req.ConversationID, parentID)
		if err != nil {
			return chainParent{}, false, "", "", err
		}
		return chainParent{id: parentID, slot: hash, depth: depth + 1}, false, "", "", nil
	}

	rootKind = req.Root
	if rootKind == "" {
		rootKind = convtree.RootKindRoot
	}
	derivedFromID = req.DerivedFromID
	if derivedFromID == "" && req.DerivedFromHead {
		derivedFromID = head
	}
	if derivedFromID == "" {
		return chainParent{slot: convtree.RootSeed(rootKind, nil)}, true, rootKind, "", nil
	}
	hash, _, err := readTreeParent(ctx, tx, req.ConversationID, derivedFromID)
	if err != nil {
		return chainParent{}, false, "", "", fmt.Errorf("derived-from: %w", err)
	}
	return chainParent{slot: convtree.RootSeed(rootKind, &hash)}, true, rootKind, derivedFromID, nil
}

// readTreeParent returns the path hash and depth of one node of the
// conversation.
func readTreeParent(ctx context.Context, tx pgx.Tx, conversationID, id string) (convtree.Hash, int, error) {
	var (
		raw   []byte
		depth int
	)
	err := tx.QueryRow(ctx, `
		SELECT path_hash, coalesce(depth, 0) FROM conversations.conversation_message
		 WHERE id = $1::uuid AND conversation_id = $2::uuid`, id, conversationID).Scan(&raw, &depth)
	if errors.Is(err, pgx.ErrNoRows) {
		return convtree.Hash{}, 0, fmt.Errorf("parent %s not in conversation %s", id, conversationID)
	}
	if err != nil {
		return convtree.Hash{}, 0, fmt.Errorf("read parent %s: %w", id, err)
	}
	if raw == nil {
		return convtree.Hash{}, 0, fmt.Errorf("parent %s is not on the tree", id)
	}
	var h convtree.Hash
	if len(raw) != len(h) {
		return convtree.Hash{}, 0, fmt.Errorf("parent %s has a malformed path hash", id)
	}
	copy(h[:], raw)
	return h, depth, nil
}

// loadChainNodes reads the chain's hashes the conversation already holds.
func loadChainNodes(ctx context.Context, tx pgx.Tx, conversationID string, hashes []convtree.Hash) (map[convtree.Hash]chainExisting, error) {
	out := make(map[convtree.Hash]chainExisting, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	raw := make([][]byte, len(hashes))
	for i, h := range hashes {
		raw[i] = h[:]
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, path_hash, ordinal, coalesce(depth, 0), created_at
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash = ANY($2::bytea[])`, conversationID, raw)
	if err != nil {
		return nil, fmt.Errorf("read existing chain: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e   chainExisting
			key []byte
		)
		if err := rows.Scan(&e.id, &key, &e.ordinal, &e.depth, &e.createdAt); err != nil {
			return nil, fmt.Errorf("scan existing node: %w", err)
		}
		var h convtree.Hash
		if len(key) != len(h) {
			return nil, fmt.Errorf("existing node %s has a malformed path hash", e.id)
		}
		copy(h[:], key)
		out[h] = e
	}
	return out, rows.Err()
}

// fenceLease refuses the write when lease is no longer live for THIS
// conversation: a lease held on another conversation proves nothing about this
// one. FOR SHARE on the lease row is what holds a takeover's UPDATE out for the
// rest of the transaction.
func fenceLease(ctx context.Context, tx pgx.Tx, conversationID string, lease Lease) error {
	if !lease.Held() {
		return nil
	}
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid AND holder = $2 AND token = $3::uuid
		   AND expires_at > now() FOR SHARE`, conversationID, lease.Holder, lease.Token).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lease check: %w", err)
	}
	return nil
}

// nullString renders s as a NULL for an empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// boolInt renders b as the 0/1 an integer counter adds.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
