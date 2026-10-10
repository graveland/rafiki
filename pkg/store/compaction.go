// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// Kinds written by the compaction boundary path. A compaction appends a
// summary row followed by copies of the tail messages the summary did not
// cover, then moves the conversation's resume horizon to the summary.
const (
	KindCompactionSummary = "compaction_summary"
	KindCompactionTail    = "compaction_tail"
	// KindClear marks the row a /clear opens a fresh working set with. For a
	// fundi conversation it is a synthetic user row written by AppendClear;
	// for a claude conversation capture tags the first divergent head with it.
	KindClear = "clear"
)

// ClearBoundaryText is the content of the synthetic user row a fundi /clear
// opens the fresh working set with. The working set must be non-empty (Continue
// refuses an empty one, and the next ordinal derives from it), and a user turn
// is what the next prompt merges into at request assembly. It lives in
// slashcmd, a stdlib-only package, so pgx-free readers (pkg/recall) can match it.
const ClearBoundaryText = slashcmd.ClearBoundaryText

// IsClearBoundary reports whether m is the synthetic row AppendClear writes, as
// opposed to a claude clear row, which is the real first message of the new head
// and shares the kind. Full-history readers render the former as a divider.
func IsClearBoundary(m Message) bool {
	if m.Kind == nil || *m.Kind != KindClear || m.Param.Role != "user" || len(m.Param.Content) != 1 {
		return false
	}
	t := m.Param.Content[0].OfText
	return t != nil && t.Text == ClearBoundaryText
}

// LoadWorking returns the conversation's working set: the messages at and after
// the resume horizon, in ordinal order. It is the only horizon-filtered read;
// Load stays full.
func (m *Messages) LoadWorking(ctx context.Context, conversationID string) ([]Message, error) {
	return m.load(ctx, conversationID, true)
}

// AppendCompaction writes a compaction boundary: a summary row at the next
// ordinal, copies of tail at the ordinals after it, and a new resume horizon
// pointing at the summary. Nothing is deleted or renumbered; the call only
// appends. It returns the summary's ordinal.
//
// The whole write is one transaction. The conversation row is locked FOR UPDATE
// first, which serialises concurrent compactors, and every insert carries the
// lease guard when a lease is held. Any error rolls back, leaving the horizon
// and the row set exactly as they were.
func (m *Messages) AppendCompaction(ctx context.Context, conversationID string, summary anthropic.MessageParam, replacedTokens int, tail []Message) (int, error) {
	return m.appendBoundary(ctx, conversationID, KindCompactionSummary, summary, &replacedTokens, tail)
}

// AppendClear writes a clear boundary: boundary at the next ordinal, tagged
// KindClear, with the resume horizon moved onto it so the working set restarts
// there. It is AppendCompaction with no summary content and no tail — nothing is
// deleted or renumbered, and the whole write is one lease-fenced transaction.
// It returns the boundary's ordinal.
//
// boundary must carry real content: an empty working set cannot be continued,
// and the next AppendUser would take ordinal 0 over a row that already exists.
func (m *Messages) AppendClear(ctx context.Context, conversationID string, boundary anthropic.MessageParam) (int, error) {
	return m.appendBoundary(ctx, conversationID, KindClear, boundary, nil, nil)
}

// appendBoundary is the shared transaction behind AppendCompaction and
// AppendClear: a boundary row of the given kind at the next ordinal, copies of
// tail after it, and the horizon moved onto the boundary.
func (m *Messages) appendBoundary(ctx context.Context, conversationID, kind string, boundary anthropic.MessageParam, replacedTokens *int, tail []Message) (int, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("append compaction: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int
	err = tx.QueryRow(ctx, `SELECT 1 FROM conversations.conversation WHERE id = $1::uuid FOR UPDATE`, conversationID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("append compaction: conversation %s not found", conversationID)
	}
	if err != nil {
		return 0, fmt.Errorf("append compaction: lock conversation: %w", err)
	}

	var summaryOrdinal int
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(ordinal), -1) + 1 FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid`, conversationID).Scan(&summaryOrdinal); err != nil {
		return 0, fmt.Errorf("append compaction: next ordinal: %w", err)
	}

	summaryContent, err := json.Marshal(boundary.Content)
	if err != nil {
		return 0, fmt.Errorf("append compaction: marshal summary: %w", err)
	}
	var inTok any
	if replacedTokens != nil {
		inTok = *replacedTokens
	}
	if err := m.compactInsert(ctx, tx, conversationID, summaryOrdinal, string(boundary.Role),
		jsonbSafe(summaryContent), toolUseIDs(boundary), inTok, nil, nil, kind); err != nil {
		return 0, err
	}

	for i, t := range tail {
		content, err := json.Marshal(t.Param.Content)
		if err != nil {
			return 0, fmt.Errorf("append compaction: marshal tail %d: %w", i, err)
		}
		var stopReason any
		if t.StopReason != "" {
			stopReason = t.StopReason
		}
		if err := m.compactInsert(ctx, tx, conversationID, summaryOrdinal+1+i, string(t.Param.Role),
			jsonbSafe(content), toolUseIDs(t.Param), nil, nil, stopReason, KindCompactionTail); err != nil {
			return 0, err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE conversations.conversation SET resume_from_ordinal = $2 WHERE id = $1::uuid`,
		conversationID, summaryOrdinal); err != nil {
		return 0, fmt.Errorf("append compaction: set horizon: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("append compaction: commit: %w", err)
	}
	return summaryOrdinal, nil
}

// compactInsert inserts one compaction row inside tx. There is deliberately no
// ON CONFLICT: the conversation row lock makes an ordinal conflict impossible,
// so a conflict is a bug that must surface rather than be silently swallowed.
// A zero-rows result means the lease guard refused the write (a stale holder
// that woke after takeover); with a held lease that is reported as ErrLeaseLost,
// otherwise as the affected-rows bug it is.
func (m *Messages) compactInsert(ctx context.Context, tx pgx.Tx, conversationID string, ordinal int, role string, content []byte, ids []string, inTok, outTok, stopReason any, kind string) error {
	var (
		tag pgconn.CommandTag
		err error
	)
	if m.lease.Held() {
		tag, err = tx.Exec(ctx, compactInsertFencedSQL,
			conversationID, ordinal, role, content, ids, inTok, outTok, stopReason, kind,
			m.lease.Holder, m.lease.Token)
	} else {
		tag, err = tx.Exec(ctx, compactInsertSQL,
			conversationID, ordinal, role, content, ids, inTok, outTok, stopReason, kind)
	}
	if err != nil {
		return fmt.Errorf("append compaction: insert at ordinal %d: %w", ordinal, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if m.lease.Held() {
		valid, verr := NewLeases(m.pool).Valid(ctx, m.lease)
		if verr != nil {
			return fmt.Errorf("append compaction: insert at ordinal %d: lease check: %w", ordinal, verr)
		}
		if !valid {
			return fmt.Errorf("append compaction: %w", ErrLeaseLost)
		}
	}
	return fmt.Errorf("append compaction: insert at ordinal %d affected %d rows", ordinal, tag.RowsAffected())
}

const compactInsertSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, tool_use_ids, input_tokens, output_tokens, stop_reason, kind)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)`

// compactInsertFencedSQL is compactInsertSQL with the lease guard, evaluated in
// the same statement as the insert so a stalled writer that wakes after
// takeover writes nothing.
const compactInsertFencedSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, tool_use_ids, input_tokens, output_tokens, stop_reason, kind)
SELECT $1::uuid, $2, $3, $4, $5, $6, $7, $8, $9
 WHERE EXISTS (
   SELECT 1 FROM conversations.conversation_lease
    WHERE conversation_id = $1::uuid AND holder = $10 AND token = $11::uuid
      AND expires_at > now())`
