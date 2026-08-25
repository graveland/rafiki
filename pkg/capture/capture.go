// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"
)

type CaptureStore struct {
	pool  *pgxpool.Pool
	lease store.Lease
}

func NewCaptureStore(pool *pgxpool.Pool) *CaptureStore { return &CaptureStore{pool: pool} }

// WithLease returns a copy of s whose message writes are guarded by l. A zero
// Lease means unfenced — the client-driven proxy path, which no daemon resumes.
func (s *CaptureStore) WithLease(l store.Lease) *CaptureStore {
	cp := *s
	cp.lease = l
	return &cp
}

// appendMessage is the single guarded insert both message write sites use.
// inTok, outTok and stopReason are nil/nil-able for the request-decomposition
// path, which has no usage to record.
//
// A returned ErrLeaseLost is deliberately NOT retryable: isRetryableDB only
// classifies DeadlineExceeded, net.OpError and SQLSTATE 40P01/40001, so retryDB
// surfaces it on the first attempt. Do not add it to the retryable set — a lost
// lease is an answer, not a blip.
func (s *CaptureStore) appendMessage(ctx context.Context, convID string, ordinal int, role string, content []byte, inTok, outTok *int64, stopReason any) error {
	var tag pgconn.CommandTag
	var err error
	if s.lease.Held() {
		tag, err = s.pool.Exec(ctx, captureAppendFencedSQL,
			convID, ordinal, role, content, inTok, outTok, stopReason,
			s.lease.Holder, s.lease.Token)
	} else {
		tag, err = s.pool.Exec(ctx, captureAppendSQL,
			convID, ordinal, role, content, inTok, outTok, stopReason)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 && s.lease.Held() {
		valid, verr := store.NewLeases(s.pool).Valid(ctx, s.lease)
		if verr != nil {
			return verr
		}
		if !valid {
			return store.ErrLeaseLost
		}
	}
	return nil
}

// appendMessageForTest exposes appendMessage to this package's tests.
func (s *CaptureStore) appendMessageForTest(ctx context.Context, convID string, ordinal int, role string, content []byte) error {
	return s.appendMessage(ctx, convID, ordinal, role, content, nil, nil, nil)
}

const captureAppendSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, input_tokens, output_tokens, stop_reason)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
ON CONFLICT (conversation_id, ordinal) DO NOTHING`

// captureAppendFencedSQL is captureAppendSQL with the lease guard. The EXISTS
// clause is evaluated in the SAME statement as the insert, which is what makes
// a stalled writer that wakes after takeover write nothing.
const captureAppendFencedSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, input_tokens, output_tokens, stop_reason)
SELECT $1::uuid, $2, $3, $4, $5, $6, $7
 WHERE EXISTS (
   SELECT 1 FROM conversations.conversation_lease
    WHERE conversation_id = $1::uuid AND holder = $8 AND token = $9::uuid
      AND expires_at > now())
ON CONFLICT (conversation_id, ordinal) DO NOTHING`

type ConversationRef struct {
	ID               string // empty → insert a new conversation and return its id
	OriginEntrypoint string
	DrivenBy         string // "server" | "client"
	OwnerUserID      string // conversations.users.id; empty → unattributed (SQL NULL)
	Persona          string
	Model            string
	Name             string
	ExternalRef      string
	Cwd              string
	RepoRoot         string
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
			`INSERT INTO conversations.conversation (id, owner_user_id, persona, model, origin_entrypoint, driven_by, name, external_ref, cwd, repo_root)
			 VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (id) DO NOTHING`,
			ref.ID, nullUUID(ref.OwnerUserID), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, nullify(ref.Name), nullify(ref.ExternalRef),
			nullify(ref.Cwd), nullify(ref.RepoRoot),
		)
		if err != nil {
			return "", err
		}
		return ref.ID, nil
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (owner_user_id, persona, model, origin_entrypoint, driven_by, name, external_ref, cwd, repo_root)
		 VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
		nullUUID(ref.OwnerUserID), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, nullify(ref.Name), nullify(ref.ExternalRef),
		nullify(ref.Cwd), nullify(ref.RepoRoot),
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
			`INSERT INTO conversations.conversation (owner_user_id, persona, model, origin_entrypoint, driven_by, name, external_ref, cwd, repo_root)
			 VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (external_ref, driven_by) WHERE external_ref IS NOT NULL DO NOTHING
			 RETURNING id::text`,
			nullUUID(ref.OwnerUserID), nullify(ref.Persona), nullify(ref.Model), ref.OriginEntrypoint, ref.DrivenBy, nullify(ref.Name), ref.ExternalRef,
			nullify(ref.Cwd), nullify(ref.RepoRoot),
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
		OwnerUserID: ref.OwnerUserID, Persona: ref.Persona, Model: ref.Model, Name: ref.Name, ExternalRef: ref.ExternalRef,
		Cwd: ref.Cwd, RepoRoot: ref.RepoRoot,
	})
}

type TurnIntent struct {
	ConversationID string
	Ordinal        int
	Model          string
	Request        []byte // marshaled request JSON
	Source         string // inbound entrypoint: 'claude' | 'tui' | 'slack' | 'diagnose' | ...
	AuthorUserID   string // conversations.users.id; empty → unattributed (SQL NULL)
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
		`INSERT INTO conversations.conversation_turn (conversation_id, ordinal, status, model, request, source, author_user_id, author_kind, prefix_hash, protocol)
		 VALUES ($1,$2,'pending',$3,$4,$5,$6::uuid,$7,$8,$9) RETURNING id::text, created_at`,
		t.ConversationID, t.Ordinal, nullify(t.Model), nullifyBytes(jsonbSafe(t.Request)),
		nullify(t.Source), nullUUID(t.AuthorUserID), nullify(t.AuthorKind), nullify(t.PrefixHash), protocol).Scan(&turnID, &createdAt)
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
	Model               string // served model from the response; empty keeps the intent model
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
	return retryDB(ctx, "completeTurn", func(ctx context.Context) error {
		tag, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn
			    SET status='complete', response=$3, stop_reason=$4, upstream=$5,
			        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10,
			        model=COALESCE(NULLIF($11,''), model)
			  WHERE id=$1::uuid AND created_at=$2`,
			r.TurnID, r.CreatedAt, nullifyBytes(jsonbSafe(r.Response)), nullify(r.StopReason), nullify(r.Upstream),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.LatencyMS, r.Model)
		if err != nil {
			return err
		}
		// The (id, created_at) pair must match exactly one chunk-resident row; zero
		// rows means a skewed/stale key silently no-op'd the capture write.
		if n := tag.RowsAffected(); n != 1 {
			return fmt.Errorf("complete-turn: expected 1 row, updated %d (stale/skewed key)", n)
		}
		// Backfill conversation.model from the response model: InsertTurnIntent
		// writes the request body's model first-seen, but for client-driven sessions
		// the first outbound request body may carry a client-side default (e.g.
		// "claude-haiku-4-5-20251001") that differs from the actual served model
		// the user intended (e.g. "claude-opus-5", set later via /model or
		// --model). Using the response model is authoritative. Best-effort: a
		// failed backfill must not fail the turn.
		if r.Model != "" {
			_, _ = s.pool.Exec(ctx, `UPDATE conversations.conversation
				SET model = $3, updated_at = now()
				WHERE id = (
					SELECT conversation_id FROM conversations.conversation_turn
					WHERE id = $1::uuid AND created_at = $2
				) AND model IS NULL`,
				r.TurnID, r.CreatedAt, r.Model)
		}
		return nil
	})
}

func (s *CaptureStore) FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error {
	return retryDB(ctx, "failTurn", func(ctx context.Context) error {
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
	})
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
	err := retryDB(ctx, "decomposeRequest", func(ctx context.Context) error {
		// 1. Upsert each message verbatim, ordinal = index. Lenient: DO NOTHING (no divergence check).
		for i, m := range req.Messages {
			content := m.Content
			if len(content) == 0 {
				content = json.RawMessage(`null`)
			}
			if err := s.appendMessage(ctx, convID, i, m.Role, jsonbSafe(content), nil, nil, nil); err != nil {
				return fmt.Errorf("decompose: insert message %d: %w", i, err)
			}
		}
		// 2. Record the turn's prefix metadata (prefix_content on-change +
		//    cache_breakpoints). Factored out so the in-process path can reuse it
		//    without the message inserts above.
		return s.StoreTurnPrefix(ctx, convID, turnID, createdAt, reqBody, prefixHash)
	})
	return len(req.Messages), err
}

// StoreTurnPrefix records a turn's cache_breakpoints (always) and its
// prefix_content (only when the static prefix changed from the previous turn,
// keyed by prefixHash — sparse, on-change). It does NOT touch
// conversation_message rows, so it is the safe unit for the in-process (direct)
// path: that path writes messages at conversation-history ordinals, which don't
// align with DecomposeRequest's index-in-request ordinals once history is merged
// or trimmed. Best-effort: returns an error for the caller to Warn; never panics.
func (s *CaptureStore) StoreTurnPrefix(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) error {
	changed, err := s.prefixChanged(ctx, convID, createdAt, prefixHash)
	if err != nil {
		return err
	}
	envelope, breakpoints, err := splitEnvelopeAndBreakpoints(reqBody)
	if err != nil {
		return fmt.Errorf("store turn prefix: %w", err)
	}
	// cache_breakpoints is a per-request property (always recorded); prefix_content
	// is stored only when the envelope changed from the previous turn.
	if changed {
		if _, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn SET prefix_content=$3, cache_breakpoints=$4
			  WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, jsonbSafe(envelope), breakpoints); err != nil {
			return fmt.Errorf("store turn prefix: prefix_content: %w", err)
		}
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation_turn SET cache_breakpoints=$3 WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt, breakpoints); err != nil {
		return fmt.Errorf("store turn prefix: breakpoints: %w", err)
	}
	return nil
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
func splitEnvelopeAndBreakpoints(reqBody []byte) (envelope []byte, breakpoints []byte, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &m); err != nil {
		return nil, nil, fmt.Errorf("split envelope: unmarshal request: %w", err)
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
	envelope, err = json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("split envelope: marshal envelope: %w", err)
	}
	breakpoints, err = json.Marshal(idxs)
	if err != nil {
		return nil, nil, fmt.Errorf("split envelope: marshal breakpoints: %w", err)
	}
	return envelope, breakpoints, nil
}

// messageHasCacheControl reports whether a message's content carries a
// cache_control field on any content block. It decodes content as an array of
// blocks (the shape cache_control actually appears in) and inspects each
// block's cache_control field directly, so a message whose text merely
// contains the literal substring "cache_control" is not a false positive.
// Plain-string content carries no cache_control block, so it is always false —
// even when the string text embeds the literal token. Only a genuinely
// unexpected shape falls back to a conservative substring check.
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
	// A plain string message has no per-block cache_control; decoding it as a
	// string (rather than substring-matching) avoids flagging text that merely
	// mentions the token.
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return false
	}
	// Genuinely unexpected shape — conservative substring check so capture never fails.
	return bytes.Contains(content, []byte(`"cache_control"`))
}

// jsonbSafe makes a JSON value storable in a Postgres jsonb column, which
// cannot represent U+0000: a `\u0000` escape triggers SQLSTATE 22P05 on insert.
// Captured content is arbitrary client/model text (a pasted NUL, a tool result),
// so strip U+0000 from every string. Values without the escape are returned
// verbatim — only NUL-bearing content pays the decode/re-encode. Invalid JSON is
// returned unchanged so the insert surfaces the real error.
func jsonbSafe(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // preserve integer precision through the round-trip
	var v any
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(stripNUL(v))
	if err != nil {
		return b
	}
	return out
}

// stripNUL recursively removes U+0000 from every string (and map key) in a
// decoded JSON value. json.Number and other scalars pass through unchanged.
func stripNUL(v any) any {
	switch t := v.(type) {
	case string:
		if strings.IndexByte(t, 0) < 0 {
			return t
		}
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i, e := range t {
			t[i] = stripNUL(e)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[strings.ReplaceAll(k, "\x00", "")] = stripNUL(e)
		}
		return out
	default:
		return v
	}
}

// AppendResponseMessage appends the canonical assistant Message (canonical, an
// anthropic.Message JSON) as a conversation_message at `ordinal`, content
// stored verbatim from canonical.content, plus token usage and stop_reason.
// Insert is ON CONFLICT (conversation_id, ordinal) DO NOTHING — lenient,
// best-effort, matching DecomposeRequest's message-insert semantics. Also
// sets the turn's response_ordinal to `ordinal`. `ordinal` is expected to equal
// the request's message count (the caller's cr.nextOrdinal from
// countRequestMessages), so the assistant lands right after request messages
// 0..ordinal-1.
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
	return retryDB(ctx, "appendResponse", func(ctx context.Context) error {
		if err := s.appendMessage(ctx, convID, ordinal, "assistant", jsonbSafe(content), &in, &out, nullify(stopReason)); err != nil {
			return fmt.Errorf("append response: insert: %w", err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn SET response_ordinal=$3 WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, ordinal); err != nil {
			return fmt.Errorf("append response: set response_ordinal: %w", err)
		}
		return nil
	})
}

// nullUUID renders an empty id as SQL NULL. Kept separate from nullify to
// mark the columns it feeds as UUID foreign keys, where an empty string is
// not merely "unattributed" — it is a cast error at insert time.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// ---- retry helpers --------------------------------------------------------

// dbRetryDelays defines the backoff sequence for DB retries (3 retries after
// the initial attempt). A package var (not a local literal) so tests can
// shrink it instead of sleeping through a real 1s/3s/5s sequence.
var dbRetryDelays = []time.Duration{1 * time.Second, 3 * time.Second, 5 * time.Second}

// retryDB wraps a database operation with retries on transient errors.
// Each attempt gets its own sub-context (20s timeout, bounded by ctx's own
// deadline when tighter). retryDB does NOT log the final outcome — callers
// log that at their level; it only WARNs on each retry it takes.
func retryDB(ctx context.Context, op string, fn func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		subCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := fn(subCtx)
		cancel()
		if err == nil || !isRetryableDB(err, ctx) {
			return err
		}
		if attempt >= len(dbRetryDelays) {
			return err
		}
		slog.Warn("capture: retrying db op", "op", op, "attempt", attempt+1, "error", err)
		select {
		case <-time.After(dbRetryDelays[attempt]):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// isRetryableDB reports whether a database error is transient and worth
// retrying. ctx must be checked first: if ctx.Err() != nil the parent is
// shutting down and no retry should be attempted regardless of the error
// shape — mirrors agentloop.isRetryable, and (unlike relying on retryDB's
// own select against ctx.Done()) makes cancellation deterministic rather
// than racing time.After when both fire on an already-canceled context.
func isRetryableDB(err error, ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		return false
	}
	// context.DeadlineExceeded from our sub-context is transient (the op
	// timed out, but the DB itself is reachable and may succeed next time).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Network errors.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// pgx connection/pool exhaustion errors.
	if isPgTransient(err) {
		return true
	}
	return false
}

// isPgTransient detects pgx-level transient errors: deadlocks (40P01) and
// serialization failures (40001). Connection failures already surface as
// net.OpError (caught above), so only explicit SQLSTATEs are classified here.
func isPgTransient(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.SQLState() {
	case "40P01", "40001": // deadlock_detected, serialization_failure
		return true
	default:
		return false
	}
}

// ModelTokens is one model's token totals within a conversation.
type ModelTokens struct {
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// ConversationTokens returns per-model token totals across a conversation's
// completed turns, for pricing a running cost.
//
// Grouped by model rather than summed flat because a conversation can change
// model mid-flight — a failover to OpenRouter, a caller switching lines — and
// the models price differently. Summing every turn's tokens together and
// pricing them once would silently bill the whole conversation at whichever
// model happened to be asked about.
//
// Errored and in-flight turns are excluded: a failed turn's usage is partial or
// zero, and charging for it would make the total drift from the invoice.
func (s *CaptureStore) ConversationTokens(ctx context.Context, convID string) ([]ModelTokens, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(model,''),
		        COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0)
		   FROM conversations.conversation_turn
		  WHERE conversation_id = $1::uuid AND status = 'complete'
		  GROUP BY model`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelTokens
	for rows.Next() {
		var m ModelTokens
		if err := rows.Scan(&m.Model, &m.InputTokens, &m.OutputTokens, &m.CacheReadTokens, &m.CacheCreationTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
