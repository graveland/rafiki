package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timescale/rafiki/store"
)

type CaptureStore struct{ pool *pgxpool.Pool }

func NewCaptureStore(pool *pgxpool.Pool) *CaptureStore { return &CaptureStore{pool: pool} }

type ConversationRef struct {
	ID               string // empty → insert a new conversation and return its id
	OriginEntrypoint string
	DrivenBy         string // "server" | "client"
	Owner            string
	Persona          string
	Model            string
	ExternalRef      string
}

// EnsureConversation creates the conversation row if it doesn't already exist
// and returns its id. When ref.ID is set, the caller supplies the id (e.g. one
// minted per run) and the insert is create-if-absent via ON CONFLICT DO
// NOTHING — a repeat call with the same id is a no-op. When ref.ID is empty, a
// new conversation is inserted and its DB-generated id (uuidv7() default) is
// returned. EnsureConversation does not correlate by external_ref; use
// EnsureConversationByExternalRef for the client-driven correlation path.
func (s *CaptureStore) EnsureConversation(ctx context.Context, ref ConversationRef) (string, error) {
	if ref.ID != "" {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO conversations.conversation (id, owner, persona, model, origin_entrypoint, driven_by, external_ref)
			 VALUES ($1::uuid,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO NOTHING`,
			ref.ID, nullify(ref.Owner), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, nullify(ref.ExternalRef),
		)
		if err != nil {
			return "", err
		}
		return ref.ID, nil
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (owner, persona, model, origin_entrypoint, driven_by, external_ref)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
		nullify(ref.Owner), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, nullify(ref.ExternalRef),
	).Scan(&id)
	return id, err
}

// EnsureConversationByExternalRef correlates a client session (external_ref,
// e.g. X-Rafiki-Session) to one conversation: it reuses an existing row with
// the same external_ref + driven_by, else creates a fresh conversation. Used by
// the HTTP proxy (client-driven) path; the in-process path supplies its own
// per-run id via EnsureConversation instead. Sequential-safe; the concurrent
// case is bounded by X-Rafiki-Session being per-session.
func (s *CaptureStore) EnsureConversationByExternalRef(ctx context.Context, ref ConversationRef) (string, error) {
	if ref.ExternalRef != "" {
		// Race-safe: the partial unique index on (external_ref, driven_by) makes
		// concurrent sessions collide on INSERT; the winner gets its id back, the
		// loser gets no row and falls through to SELECT the existing one.
		var id string
		err := s.pool.QueryRow(ctx,
			`INSERT INTO conversations.conversation (owner, persona, model, origin_entrypoint, driven_by, external_ref)
			 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (external_ref, driven_by) WHERE external_ref IS NOT NULL DO NOTHING
			 RETURNING id::text`,
			nullify(ref.Owner), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, ref.ExternalRef,
		).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		err = s.pool.QueryRow(ctx,
			`SELECT id::text FROM conversations.conversation
			  WHERE external_ref=$1 AND driven_by=$2 ORDER BY created_at LIMIT 1`,
			ref.ExternalRef, ref.DrivenBy).Scan(&id)
		return id, err
	}
	return s.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: ref.OriginEntrypoint, DrivenBy: ref.DrivenBy,
		Owner: ref.Owner, Persona: ref.Persona, Model: ref.Model, ExternalRef: ref.ExternalRef,
	})
}

type TurnIntent struct {
	ConversationID string
	Ordinal        int
	Model          string
	Request        []byte // marshaled request JSON
	Source         string // inbound entrypoint: 'claude' | 'tui' | 'slack' | 'diagnose' | ...
	Author         string // who-is username / slack user id (may be empty)
	AuthorKind     string // 'human' | 'agent' | 'system' (may be empty)
	PrefixHash     string // sha256 of the static cache-prefix (request minus messages); see PrefixHash
	Protocol       string // 'anthropic' (default when empty) | 'openai'; how to read the JSONB
}

// InsertTurnIntent write-aheads the turn's request row and returns its
// surrogate id plus created_at, which together are the update key for the later
// CompleteTurn/FailTurn call (the pair prunes the update to a single chunk).
// Ordinal is retained only as an ordering hint, not an update key — intra-turn
// retries and cross-run correlation must never collide on it.
func (s *CaptureStore) InsertTurnIntent(ctx context.Context, t TurnIntent) (turnID string, createdAt time.Time, err error) {
	protocol := t.Protocol
	if protocol == "" {
		protocol = string(store.ProtocolAnthropic)
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation_turn (conversation_id, ordinal, status, model, request, source, author, author_kind, prefix_hash, protocol)
		 VALUES ($1,$2,'pending',$3,$4,$5,$6,$7,$8,$9) RETURNING id::text, created_at`,
		t.ConversationID, t.Ordinal, nullify(t.Model), nullifyBytes(t.Request),
		nullify(t.Source), nullify(t.Author), nullify(t.AuthorKind), nullify(t.PrefixHash), protocol).Scan(&turnID, &createdAt)
	if err != nil {
		return "", time.Time{}, err
	}
	// Backfill conversation.model once known: client-driven conversations are
	// created from the session header with no model; the first turn's resolved
	// model fills it. First-seen wins (WHERE model IS NULL) — mid-conversation
	// model changes stay visible per-turn, never rewrite the conversation.
	// Best-effort: a failed backfill must not fail the turn, and the next
	// turn retries it (the column is still NULL).
	if t.Model != "" {
		_, _ = s.pool.Exec(ctx, `UPDATE conversations.conversation
			SET model = $2, updated_at = now()
			WHERE id = $1::uuid AND model IS NULL`, t.ConversationID, t.Model)
	}
	return turnID, createdAt, nil
}

type TurnResult struct {
	TurnID              string
	CreatedAt           time.Time
	Response            []byte
	StopReason          string
	Upstream            string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	LatencyMS           int
}

func (s *CaptureStore) CompleteTurn(ctx context.Context, r TurnResult) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation_turn
		    SET status='complete', response=$3, stop_reason=$4, upstream=$5,
		        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10
		  WHERE id=$1::uuid AND created_at=$2`,
		r.TurnID, r.CreatedAt, nullifyBytes(r.Response), nullify(r.StopReason), nullify(r.Upstream),
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.LatencyMS)
	if err != nil {
		return err
	}
	// The (id, created_at) pair must match exactly one chunk-resident row; zero
	// rows means a skewed/stale key silently no-op'd the capture write.
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("complete-turn: expected 1 row, updated %d (stale/skewed key)", n)
	}
	return nil
}

func (s *CaptureStore) FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation_turn SET status='error', error=$3
		  WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt, errMsg)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("fail-turn: expected 1 row, updated %d (stale/skewed key)", n)
	}
	return nil
}

// DecomposeRequest parses reqBody's messages[] into conversation_message rows
// (ordinal = index, content verbatim, ON CONFLICT (conversation_id,ordinal) DO NOTHING),
// stores prefix_content on this turn iff prefixHash != the previous turn's, and records
// cache-breakpoint ordinals on the turn. Best-effort: returns error for the caller to Warn,
// never panics, never alters reqBody. Returns the count of messages seen (next assistant ordinal).
func (s *CaptureStore) DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) (int, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return 0, fmt.Errorf("decompose: unmarshal request: %w", err)
	}
	// 1. Upsert each message verbatim, ordinal = index. Lenient: DO NOTHING (no divergence check).
	for i, m := range req.Messages {
		content := m.Content
		if len(content) == 0 {
			content = json.RawMessage(`null`)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (conversation_id, ordinal) DO NOTHING`,
			convID, i, m.Role, []byte(content)); err != nil {
			return 0, fmt.Errorf("decompose: insert message %d: %w", i, err)
		}
	}
	// 2. prefix_content on-change: store the request envelope (request minus messages) iff
	//    prefixHash differs from the most recent earlier turn's prefix_hash in this conversation.
	changed, err := s.prefixChanged(ctx, convID, createdAt, prefixHash)
	if err != nil {
		return len(req.Messages), err
	}
	if changed {
		envelope, breakpoints := splitEnvelopeAndBreakpoints(reqBody)
		if _, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn SET prefix_content=$3, cache_breakpoints=$4
			  WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, envelope, breakpoints); err != nil {
			return len(req.Messages), fmt.Errorf("decompose: store prefix_content: %w", err)
		}
	} else {
		// still record breakpoints (per-request property) even when envelope unchanged
		_, breakpoints := splitEnvelopeAndBreakpoints(reqBody)
		if _, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn SET cache_breakpoints=$3 WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, breakpoints); err != nil {
			return len(req.Messages), fmt.Errorf("decompose: store breakpoints: %w", err)
		}
	}
	return len(req.Messages), nil
}

// prefixChanged reports whether prefixHash differs from the most recent turn (by created_at)
// strictly before createdAt in this conversation. First turn (no prior) → changed=true.
func (s *CaptureStore) prefixChanged(ctx context.Context, convID string, createdAt time.Time, prefixHash string) (bool, error) {
	var prev string
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(prefix_hash,'') FROM conversations.conversation_turn
		  WHERE conversation_id=$1 AND created_at < $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`, convID, createdAt).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return prev != prefixHash, nil
}

// splitEnvelopeAndBreakpoints returns (request-minus-messages JSONB, cache_control breakpoint
// ordinals JSONB). Breakpoints are the indices of messages carrying a cache_control marker,
// as a JSON array; envelope is the request with "messages" removed.
func splitEnvelopeAndBreakpoints(reqBody []byte) (envelope []byte, breakpoints []byte) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &m); err != nil {
		return nil, nil
	}
	// breakpoints: index of each message whose content has a cache_control field
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	idxs := []int{}
	if raw, ok := m["messages"]; ok {
		if err := json.Unmarshal(raw, &msgs); err == nil {
			for i, mm := range msgs {
				if messageHasCacheControl(mm.Content) {
					idxs = append(idxs, i)
				}
			}
		}
	}
	delete(m, "messages")
	envelope, _ = json.Marshal(m)
	breakpoints, _ = json.Marshal(idxs)
	return envelope, breakpoints
}

// messageHasCacheControl reports whether a message's content carries a
// cache_control field on any content block. It decodes content as an array of
// blocks (the shape cache_control actually appears in) and inspects each
// block's cache_control field directly, so a message whose text merely
// contains the literal substring "cache_control" is not a false positive.
// If content isn't an array of blocks (e.g. a plain string), it falls back to
// a conservative substring check so capture never fails on unexpected shapes.
func messageHasCacheControl(content json.RawMessage) bool {
	var blocks []struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			if len(b.CacheControl) > 0 {
				return true
			}
		}
		return false
	}
	// content wasn't an array of blocks (e.g. a plain string) — fall back to a
	// conservative substring check so we never fail capture.
	return bytes.Contains(content, []byte(`"cache_control"`))
}

// AppendResponseMessage appends the canonical assistant Message (canonical, an
// anthropic.Message JSON) as a conversation_message at `ordinal`, content
// stored verbatim from canonical.content, plus token usage and stop_reason.
// Insert is ON CONFLICT (conversation_id, ordinal) DO NOTHING — lenient,
// best-effort, matching DecomposeRequest's message-insert semantics. Also
// sets the turn's response_ordinal to `ordinal`. `ordinal` is expected to be
// the nextOrdinal returned by DecomposeRequest.
func (s *CaptureStore) AppendResponseMessage(ctx context.Context, convID, turnID string, createdAt time.Time, ordinal int, canonical []byte, in, out int64, stopReason string) error {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(canonical, &msg); err != nil {
		return fmt.Errorf("append response: unmarshal canonical: %w", err)
	}
	content := msg.Content
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO conversations.conversation_message
		   (conversation_id, ordinal, role, content, input_tokens, output_tokens, stop_reason)
		 VALUES ($1,$2,'assistant',$3,$4,$5,$6)
		 ON CONFLICT (conversation_id, ordinal) DO NOTHING`,
		convID, ordinal, []byte(content), in, out, nullify(stopReason)); err != nil {
		return fmt.Errorf("append response: insert: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation_turn SET response_ordinal=$3 WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt, ordinal); err != nil {
		return fmt.Errorf("append response: set response_ordinal: %w", err)
	}
	return nil
}

// nullify maps "" to nil so empty strings persist as SQL NULL.
func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullifyBytes maps a nil/empty []byte to nil so pgx writes SQL NULL instead
// of an empty JSONB value for a not-yet-set (or intentionally omitted) column.
func nullifyBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
