// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Messages persists and loads message-granularity conversation state
// (conversations.conversation_message). Turn-granularity capture stays in
// routing.CaptureStore; library-driven sends write both.
type Messages struct{ pool *pgxpool.Pool }

func NewMessages(pool *pgxpool.Pool) *Messages { return &Messages{pool: pool} }

// Message is one stored conversation message. Content round-trips through the
// Anthropic SDK's MessageParam JSON form byte-identically (verified for text,
// tool_use and tool_result blocks), so the stored JSONB is the working state.
type Message struct {
	Ordinal    int
	Param      anthropic.MessageParam
	ToolUseIDs []string
	StopReason string // assistant rows: stop reason of the turn that produced it
}

// Load returns the conversation's messages in ordinal order.
func (m *Messages) Load(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, '')
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			msg     Message
			role    string
			content []byte
		)
		if err := rows.Scan(&msg.Ordinal, &role, &content, &msg.ToolUseIDs, &msg.StopReason); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		wire, err := json.Marshal(map[string]json.RawMessage{
			"role": json.RawMessage(`"` + role + `"`), "content": content,
		})
		if err != nil {
			return nil, fmt.Errorf("assemble message %d: %w", msg.Ordinal, err)
		}
		if err := json.Unmarshal(wire, &msg.Param); err != nil {
			return nil, fmt.Errorf("decode message %d: %w", msg.Ordinal, err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// Append persists one message at the given ordinal. Idempotent under resume:
// re-appending an ordinal that already exists is a no-op when the content
// matches, and an error when it doesn't (two histories diverged).
func (m *Messages) Append(ctx context.Context, conversationID string, ordinal int, param anthropic.MessageParam, meta *AssistantMeta) error {
	content, err := json.Marshal(param.Content)
	if err != nil {
		return fmt.Errorf("append message: marshal content: %w", err)
	}
	var inTok, outTok *int64
	stopReason := any(nil)
	if meta != nil {
		inTok, outTok = &meta.Input, &meta.Output
		if meta.StopReason != "" {
			stopReason = meta.StopReason
		}
	}
	tag, err := m.pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message
			(conversation_id, ordinal, role, content, tool_use_ids, input_tokens, output_tokens, stop_reason)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (conversation_id, ordinal) DO NOTHING`,
		conversationID, ordinal, string(param.Role), content, toolUseIDs(param), inTok, outTok, stopReason)
	if err != nil {
		return fmt.Errorf("append message %d: %w", ordinal, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Conflict: verify the existing row carries the same content (a Resume
	// replay), otherwise fail loudly rather than silently forking history.
	var existing []byte
	if err := m.pool.QueryRow(ctx, `
		SELECT content FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = $2`, conversationID, ordinal).Scan(&existing); err != nil {
		return fmt.Errorf("append message %d: conflict readback: %w", ordinal, err)
	}
	if !jsonEqual(existing, content) {
		return fmt.Errorf("append message %d: ordinal already holds different content (history diverged)", ordinal)
	}
	return nil
}

// AssistantMeta carries per-message usage and the producing turn's stop
// reason (assistant messages; resume consults StopReason to distinguish a
// clean end_turn from a max_tokens truncation).
type AssistantMeta struct {
	Input      int64
	Output     int64
	StopReason string
}

// ToolUseIDsOf extracts the tool_use / tool_result ids from a message's
// blocks — the linkage used by orphaned-tool_use recovery (persisted as the
// tool_use_ids column; computed on the fly for in-memory conversations).
func ToolUseIDsOf(param anthropic.MessageParam) []string { return toolUseIDs(param) }

// toolUseIDs extracts the tool_use / tool_result ids from a message's blocks —
// the SQL-queryable linkage used by orphaned-tool_use recovery.
func toolUseIDs(param anthropic.MessageParam) []string {
	var ids []string
	for _, block := range param.Content {
		if tu := block.OfToolUse; tu != nil && tu.ID != "" {
			ids = append(ids, tu.ID)
		}
		if tr := block.OfToolResult; tr != nil && tr.ToolUseID != "" {
			ids = append(ids, tr.ToolUseID)
		}
	}
	return ids
}

func jsonEqual(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // avoid float64 rounding making distinct numbers "equal"
		var v any
		err := dec.Decode(&v)
		return v, err
	}
	av, errA := decode(a)
	bv, errB := decode(b)
	if errA != nil || errB != nil {
		return false
	}
	ac, errA := json.Marshal(av)
	bc, errB := json.Marshal(bv)
	return errA == nil && errB == nil && string(ac) == string(bc)
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

// DeleteTailUserMessage removes the message at ordinal ONLY when it is the
// conversation's last row and a user message — the write-ahead retraction for
// a first send the API rejected as too large (the host rebuilds a smaller
// prompt and retries on the same conversation). Never usable against history
// the model has seen.
func DeleteTailUserMessage(ctx context.Context, pool *pgxpool.Pool, conversationID string, ordinal int) error {
	tag, err := pool.Exec(ctx, `
		DELETE FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = $2 AND role = 'user'
		   AND ordinal = (SELECT max(ordinal) FROM conversations.conversation_message
		                   WHERE conversation_id = $1::uuid)`, conversationID, ordinal)
	if err != nil {
		return fmt.Errorf("delete tail user message: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("delete tail user message: ordinal %d is not the tail user row", ordinal)
	}
	return nil
}
