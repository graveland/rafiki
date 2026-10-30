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
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// Messages reads message-granularity conversation state
// (conversations.conversation_message) as rows. It is the one reader that stays
// SQL: a capture report reads back the rows one turn wrote, by timestamp. Every
// tree-shaped read goes through conv.Store.

// ErrLeaseLost is returned by a guarded write whose conversation lease is no
// longer held. It means another daemon has taken over the conversation; the
// caller must stop writing AND stop acting — see the lease-lost path in
// cmd/rafikid. It is conv.ErrLeaseLost, so a conv.Backend's fence and this
// store's guards fail with one error value.
var ErrLeaseLost = conv.ErrLeaseLost

type Messages struct {
	pool *pgxpool.Pool
}

func NewMessages(pool *pgxpool.Pool) *Messages { return &Messages{pool: pool} }

// LoadSince reads the conversation's rows created at or after since, in
// ordinal order, omitting kind='compaction_tail' rows. It is a row reader, not
// a tree walk: a capture report reads back the rows one turn wrote, so the
// stored rows — not the head's path — are what it wants.
func (m *Messages) LoadSince(ctx context.Context, conversationID string, since time.Time) ([]*conv.Node, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''), kind, input_tokens, created_at, id::text
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND created_at >= $2 AND kind IS DISTINCT FROM $3
		 ORDER BY ordinal`, conversationID, since, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("load since: %w", err)
	}
	return scanMessages(conversationID, rows)
}

// scanMessages decodes rows selected with LoadSince's column list, closing rows.
func scanMessages(conversationID string, rows pgx.Rows) ([]*conv.Node, error) {
	defer rows.Close()
	var out []*conv.Node
	for rows.Next() {
		var (
			n       conv.Node
			role    string
			content []byte
			kind    *string
		)
		if err := rows.Scan(&n.Ordinal, &role, &content, &n.ToolUseIDs, &n.StopReason, &kind, &n.InputTokens, &n.CreatedAt, &n.ID); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if kind != nil {
			n.Kind = *kind
		}
		message, err := msganthropic.Decode(role, content)
		if err != nil {
			return nil, fmt.Errorf("decode message %d: %w", n.Ordinal, err)
		}
		n.ConversationID = conversationID
		n.Msg = message
		n.Content = content
		out = append(out, &n)
	}
	return out, rows.Err()
}

// Appended is the committed identity of a row an append wrote or found: its id
// and created_at as the database reported them, so a caller can publish the
// same time GetHistory will later report for the row.
type Appended struct {
	ID        string
	CreatedAt time.Time
	// Ordinal is the row's insertion sequence and Depth its distance from a
	// root; both stay zero for a caller that does not track tree position.
	Ordinal int
	Depth   int
}

// AssistantMeta carries per-message usage and the producing turn's stop
// reason (assistant messages; resume consults StopReason to distinguish a
// clean end_turn from a max_tokens truncation).
type AssistantMeta struct {
	Input      int64
	Output     int64
	StopReason string
}

// Unfinished describes a conversation with recoverable state: a pending turn
// (call never completed) and/or assistant tool_use blocks with no matching
// tool_result message (interrupted mid-tools).
type Unfinished struct {
	ConversationID string
	PendingTurns   int
	OrphanToolUses []string
	ResumeAttempts int
}

// UnfinishedConversations returns conversations in the given driven_by scope
// with recoverable state. Scoped to DrivenByServer by callers per the design:
// an orphaned pending proxy turn means the client vanished mid-stream and is
// FailTurn territory, not a resume point.
func UnfinishedConversations(ctx context.Context, pool *pgxpool.Pool, scope DrivenBy) ([]Unfinished, error) {
	rows, err := pool.Query(ctx, `
		WITH pending AS (
			SELECT t.conversation_id, count(*) AS n
			  FROM conversations.conversation_turn t
			  JOIN conversations.conversation c ON c.id = t.conversation_id
			 WHERE t.status = 'pending' AND c.driven_by = $1 AND c.status = 'active'
			 GROUP BY t.conversation_id
		),
		orphans AS (
			SELECT m.conversation_id, array_agg(DISTINCT tu) AS ids
			  FROM conversations.conversation_message m
			  JOIN conversations.conversation c ON c.id = m.conversation_id,
			       unnest(m.tool_use_ids) AS tu
			 WHERE m.role = 'assistant' AND c.driven_by = $1 AND c.status = 'active'
			   AND NOT EXISTS (
				SELECT 1 FROM conversations.conversation_message r
				 WHERE r.conversation_id = m.conversation_id
				   AND r.role = 'user' AND r.tool_use_ids @> ARRAY[tu])
			 GROUP BY m.conversation_id
		)
		SELECT coalesce(p.conversation_id, o.conversation_id)::text,
		       coalesce(p.n, 0), coalesce(o.ids, '{}'),
		       coalesce(c.resume_attempts, 0)
		  FROM pending p
		  FULL OUTER JOIN orphans o ON o.conversation_id = p.conversation_id
		  JOIN conversations.conversation c ON c.id = coalesce(p.conversation_id, o.conversation_id)`,
		string(scope))
	if err != nil {
		return nil, fmt.Errorf("unfinished conversations: %w", err)
	}
	defer rows.Close()
	var out []Unfinished
	for rows.Next() {
		var u Unfinished
		if err := rows.Scan(&u.ConversationID, &u.PendingTurns, &u.OrphanToolUses, &u.ResumeAttempts); err != nil {
			return nil, fmt.Errorf("scan unfinished: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetConversationStatus updates the conversation's status column (e.g.
// ConversationFailed when the Resume cap is exceeded).
func SetConversationStatus(ctx context.Context, pool *pgxpool.Pool, conversationID string, status ConversationStatus) error {
	tag, err := pool.Exec(ctx, `UPDATE conversations.conversation
		SET status = $2, updated_at = now() WHERE id = $1::uuid`, conversationID, string(status))
	if err != nil {
		return fmt.Errorf("set conversation status: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("set conversation status: conversation %s not found", conversationID)
	}
	return nil
}

// IncrementResumeAttempts bumps and returns the conversation's resume counter
// (the per-conversation Resume cap from the design's recovery semantics).
func IncrementResumeAttempts(ctx context.Context, pool *pgxpool.Pool, conversationID string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		UPDATE conversations.conversation
		   SET resume_attempts = coalesce(resume_attempts, 0) + 1, updated_at = now()
		 WHERE id = $1::uuid RETURNING resume_attempts`, conversationID).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("increment resume attempts: conversation %s not found", conversationID)
		}
		return 0, fmt.Errorf("increment resume attempts: %w", err)
	}
	return n, nil
}

// ResetResumeAttempts zeroes the counter after a successful resume so routine
// interruptions don't accumulate into a permanent failure of long-lived
// conversations — the cap counts consecutive failed recoveries only.
func ResetResumeAttempts(ctx context.Context, pool *pgxpool.Pool, conversationID string) error {
	_, err := pool.Exec(ctx, `UPDATE conversations.conversation
		SET resume_attempts = 0, updated_at = now() WHERE id = $1::uuid`, conversationID)
	if err != nil {
		return fmt.Errorf("reset resume attempts: %w", err)
	}
	return nil
}
