// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Messages persists and loads message-granularity conversation state
// (conversations.conversation_message). Turn-granularity capture stays in
// pkg/capture CaptureStore; library-driven sends write both.
// ErrLeaseLost is returned by a guarded write whose conversation lease is no
// longer held. It means another daemon has taken over the conversation; the
// caller must stop writing AND stop acting — see the lease-lost path in
// cmd/rafikid.
var ErrLeaseLost = errors.New("store: conversation lease lost")

type Messages struct {
	pool  *pgxpool.Pool
	lease Lease
}

func NewMessages(pool *pgxpool.Pool) *Messages { return &Messages{pool: pool} }

// WithLease returns a copy of m whose writes are guarded by l. A zero Lease
// means unfenced, which is the behaviour every caller had before fencing
// existed.
func (m *Messages) WithLease(l Lease) *Messages {
	cp := *m
	cp.lease = l
	return &cp
}

// Message is one stored conversation message. Content round-trips through the
// Anthropic SDK's MessageParam JSON form byte-identically (verified for text,
// tool_use and tool_result blocks), so the stored JSONB is the working state.
type Message struct {
	// ID is the row's id; empty for an in-memory conversation's rows unless
	// the caller assigns one.
	ID string
	// Ordinal is the row's insertion sequence: gap-free, never reused, and
	// unrelated to its position on the working path.
	Ordinal    int
	Param      anthropic.MessageParam
	ToolUseIDs []string
	StopReason string // assistant rows: stop reason of the turn that produced it
	// Kind tags a row as something other than an ordinary turn message. Two
	// values are written today: "compaction_summary" (the root a compaction
	// hangs its tail from, by pkg/capture or by fundi's compaction) and
	// "compaction_tail" (the post-compaction tail fundi appends — copies of
	// earlier rows, which every full-history reader must skip; see
	// docs/reference/fundi-compaction.md). A clear's root row also carries
	// "clear": for fundi it is the synthetic boundary row AppendClear writes,
	// for a claude conversation the real first message of the new head. Nil on
	// every ordinary row.
	Kind *string
	// InputTokens is set only on a kind='compaction_summary' row: the
	// approximate size of the context that boundary replaced. Nil otherwise
	// (including on a kind='clear' or kind='compaction_tail' row, which carry
	// no size).
	InputTokens *int
	// CreatedAt is the row's created_at — transaction-start time, so every row
	// one transaction wrote shares it (a compaction or clear boundary and the
	// tail copied after it). It is what a durable event's ts reports, and
	// because it can repeat, a time cursor pages on whole groups rather than
	// single rows (LoadPage).
	CreatedAt time.Time
}

// Load returns the conversation's FULL message history in ordinal order — every
// node ever written, including the ones the working path no longer reaches.
// GetHistory, `rafiki logs` and `Controller.dbRecent` (agent_view) all render
// the pre-compaction history with the boundary inline. The working set every
// send, orphan repair and prefill classifier uses is LoadWorking, the path from
// the head to its root.
func (m *Messages) Load(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''), kind, input_tokens, created_at, id::text
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	return scanMessages(rows)
}

// HistoryPage bounds a LoadPage read. Every field is optional; the zero value
// reads the whole history.
type HistoryPage struct {
	// AfterOrdinal keeps rows whose ordinal is strictly greater.
	AfterOrdinal *int
	// Before keeps rows whose created_at is strictly earlier.
	Before *time.Time
	// Limit keeps only the newest N rows left after the other bounds, widened
	// backwards so the page never splits rows that share a created_at: a
	// client pages with the oldest ts it received as the next Before, and a
	// split group's older half would carry that same ts and be skipped for
	// good. A page can therefore hold more than Limit rows. Limit must be
	// positive.
	Limit *int
}

// historyPageSQL is LoadPage's one statement. The three bounds ride as
// nullable parameters ($2 after_ordinal, $3 before, $4 limit), and $5 is the
// compaction_tail kind, whose rows render no event and so never count toward
// the limit. edge is the oldest row the limit keeps, found by walking the
// (conversation_id, ordinal) index backwards; floor is where that row's
// same-created_at run begins — one past the newest in-bounds row older than
// edge with a different created_at — so the widening is exactly "extend the
// page back over adjacent rows sharing the edge's timestamp". With no limit,
// or fewer rows than the limit, floor has no row and nothing is cut.
const historyPageSQL = `
	WITH edge AS (
		SELECT ordinal, created_at
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND kind IS DISTINCT FROM $5
		   AND ($2::int IS NULL OR ordinal > $2::int)
		   AND ($3::timestamptz IS NULL OR created_at < $3::timestamptz)
		 ORDER BY ordinal DESC
		OFFSET $4::int - 1 LIMIT 1
	), floor AS (
		SELECT coalesce((
			SELECT max(m.ordinal) + 1
			  FROM conversations.conversation_message m, edge
			 WHERE m.conversation_id = $1::uuid AND m.kind IS DISTINCT FROM $5
			   AND ($2::int IS NULL OR m.ordinal > $2::int)
			   AND ($3::timestamptz IS NULL OR m.created_at < $3::timestamptz)
			   AND m.ordinal < edge.ordinal AND m.created_at <> edge.created_at
		), -1) AS ordinal
		 WHERE $4::int IS NOT NULL AND EXISTS (SELECT 1 FROM edge)
	)
	SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''), kind, input_tokens, created_at, id::text
	  FROM conversations.conversation_message
	 WHERE conversation_id = $1::uuid AND kind IS DISTINCT FROM $5
	   AND ($2::int IS NULL OR ordinal > $2::int)
	   AND ($3::timestamptz IS NULL OR created_at < $3::timestamptz)
	   AND ordinal >= coalesce((SELECT ordinal FROM floor), -1)
	 ORDER BY ordinal`

// LoadPage reads one page of the conversation's FULL history (not
// horizon-bounded, like Load) in ordinal order, bounded by page in the
// database so a backward page costs the rows it returns, not the whole
// conversation. It omits kind='compaction_tail' rows: they are copies of
// earlier rows that render no event, so counting them would short a page.
func (m *Messages) LoadPage(ctx context.Context, conversationID string, page HistoryPage) ([]Message, error) {
	if page.Limit != nil && *page.Limit <= 0 {
		return nil, fmt.Errorf("load history page: limit %d is not positive", *page.Limit)
	}
	rows, err := m.pool.Query(ctx, historyPageSQL, conversationID,
		page.AfterOrdinal, page.Before, page.Limit, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("load history page: %w", err)
	}
	return scanMessages(rows)
}

// LoadSince reads the conversation's rows created at or after since, in
// ordinal order, omitting kind='compaction_tail' rows.
func (m *Messages) LoadSince(ctx context.Context, conversationID string, since time.Time) ([]Message, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT ordinal, role, content, coalesce(tool_use_ids, '{}'), coalesce(stop_reason, ''), kind, input_tokens, created_at, id::text
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND created_at >= $2 AND kind IS DISTINCT FROM $3
		 ORDER BY ordinal`, conversationID, since, KindCompactionTail)
	if err != nil {
		return nil, fmt.Errorf("load since: %w", err)
	}
	return scanMessages(rows)
}

// scanMessages decodes rows selected with load's column list, closing rows.
func scanMessages(rows pgx.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			msg     Message
			role    string
			content []byte
		)
		if err := rows.Scan(&msg.Ordinal, &role, &content, &msg.ToolUseIDs, &msg.StopReason, &msg.Kind, &msg.InputTokens, &msg.CreatedAt, &msg.ID); err != nil {
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

// AppendNode appends one message under the conversation's head and returns the
// committed row's identity. A conversation with no head yet starts a root.
//
// Idempotent under replay: a node the conversation already holds is matched by
// its path hash, so re-appending it writes nothing and returns the EXISTING
// row's identity — and the head still moves onto it, which is what makes a
// resumed history converge on the same working path.
//
// providerMessageID is the upstream message id for an assistant reply ("" for
// every other row); it is recorded for reconciliation with the provider.
func (m *Messages) AppendNode(ctx context.Context, conversationID string, param anthropic.MessageParam, meta *AssistantMeta, providerMessageID string) (Appended, error) {
	content, err := json.Marshal(param.Content)
	if err != nil {
		return Appended{}, fmt.Errorf("append message: marshal content: %w", err)
	}
	node := ChainNode{
		Role:              string(param.Role),
		Content:           jsonbSafe(content),
		ToolUseIDs:        toolUseIDs(param),
		ProviderMessageID: providerMessageID,
	}
	if meta != nil {
		node.InputTokens, node.OutputTokens = &meta.Input, &meta.Output
		node.StopReason = meta.StopReason
	}
	result, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conversationID, ParentFromHead: true, Nodes: []ChainNode{node},
	})
	if err != nil {
		return Appended{}, fmt.Errorf("append message: %w", err)
	}
	return result.Nodes[0], nil
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
