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

// ErrOrdinalOccupied reports that a response could not be written because its
// ordinal already held a different message. Request-message inserts stay
// lenient (a replayed prefix re-inserts identical content and first-seen wins),
// but a second assistant reply at one ordinal is always a lost response: nine
// such collisions dropped 22 model replies before threads were separated, and
// nothing reported it.
//
// Residual case: the conflict test is content-only (IS DISTINCT FROM), so an
// occupant whose content is jsonb-equal to the incoming reply stays a silent
// success. That needs a client retry re-emitting byte-identical content and is
// not covered here.
var ErrOrdinalOccupied = errors.New("capture: ordinal already occupied by a different message")

// appendMessage is the single guarded insert both message write sites use.
// inTok, outTok, stopReason and kind are nil/nil-able for the
// request-decomposition path's ordinary rows (kind is set only on a
// compaction-summary boundary row).
//
// An ordinal that is already occupied is not an error: both statements' ON
// CONFLICT (conversation_id, ordinal) DO NOTHING silently drops the new row and
// the occupant's first-seen content wins — accepted leniency for rewound and
// short requests (design §3); capture is best-effort.
//
// A returned ErrLeaseLost is deliberately NOT retryable: isRetryableDB only
// classifies DeadlineExceeded, net.OpError and SQLSTATE 40P01/40001, so retryDB
// surfaces it on the first attempt. Do not add it to the retryable set — a lost
// lease is an answer, not a blip.
func (s *CaptureStore) appendMessage(ctx context.Context, convID string, ordinal int, role string, content []byte, inTok, outTok *int64, stopReason any, kind any) error {
	var tag pgconn.CommandTag
	var err error
	if s.lease.Held() {
		tag, err = s.pool.Exec(ctx, captureAppendFencedSQL,
			convID, ordinal, role, content, inTok, outTok, stopReason, kind,
			s.lease.Holder, s.lease.Token)
	} else {
		tag, err = s.pool.Exec(ctx, captureAppendSQL,
			convID, ordinal, role, content, inTok, outTok, stopReason, kind)
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
	return s.appendMessage(ctx, convID, ordinal, role, content, nil, nil, nil, nil)
}

// appendMessageStrict is appendMessage that distinguishes a conflict from a
// lost lease. appendMessage cannot: both yield RowsAffected() == 0.
func (s *CaptureStore) appendMessageStrict(ctx context.Context, convID string, ordinal int, role string, content []byte, inTok, outTok *int64, stopReason, kind any) error {
	err := s.appendMessage(ctx, convID, ordinal, role, content, inTok, outTok, stopReason, kind)
	if err != nil {
		return err
	}
	var occupied bool
	if qerr := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message
		   WHERE conversation_id=$1::uuid AND ordinal=$2 AND content IS DISTINCT FROM $3::jsonb)`,
		convID, ordinal, content).Scan(&occupied); qerr != nil {
		return qerr
	}
	if occupied {
		return fmt.Errorf("%w: conversation %s ordinal %d", ErrOrdinalOccupied, convID, ordinal)
	}
	return nil
}

const captureAppendSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, input_tokens, output_tokens, stop_reason, kind)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (conversation_id, ordinal) DO NOTHING`

// captureAppendFencedSQL is captureAppendSQL with the lease guard. The EXISTS
// clause is evaluated in the SAME statement as the insert, which is what makes
// a stalled writer that wakes after takeover write nothing.
const captureAppendFencedSQL = `
INSERT INTO conversations.conversation_message
	(conversation_id, ordinal, role, content, input_tokens, output_tokens, stop_reason, kind)
SELECT $1::uuid, $2, $3, $4, $5, $6, $7, $8
 WHERE EXISTS (
   SELECT 1 FROM conversations.conversation_lease
    WHERE conversation_id = $1::uuid AND holder = $9 AND token = $10::uuid
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

// ResolveThreadConversation returns the conversation row a thread's turns
// belong to. threadID is the conversation_turn.id that begins the thread, or
// "" for the session's root thread.
//
// The root thread keeps the bare session external_ref, so every existing
// conversation, every cost rollup keyed on external_ref
// (pkg/insights/subtree.go) and every `rafiki logs <child>` is unchanged. A
// non-root thread gets "<session>:<threadID>", which is a distinct row and
// therefore a distinct ordinal space. That separation is the whole fix: the
// dropped replies, the phantom compaction boundaries and the ordinal
// collisions are one bug, concurrent writers sharing an ordinal space.
func (s *CaptureStore) ResolveThreadConversation(ctx context.Context, ref ConversationRef, threadID string) (string, error) {
	if threadID != "" && ref.ExternalRef != "" {
		ref.ExternalRef = ref.ExternalRef + ":" + threadID
	}
	return s.EnsureConversationByExternalRef(ctx, ref)
}

// ThreadOfPredecessorInSession is ThreadOfPredecessor across every conversation
// belonging to one session: the root ("<session>") and each branch
// ("<session>:<threadID>"). A thread's first turn chains to a message on the
// root conversation; its second chains to one on its own branch, so a lookup
// scoped to the root alone finds nothing and would restart the thread on every
// turn.
//
// The predecessor's thread_id is returned verbatim: "" means the predecessor
// IS the session's main thread, whose turns carry thread_id NULL by
// convention (RecordThread), so the caller keeps the conversation on the bare
// session external_ref. Only a non-NULL thread_id routes to a branch. A miss
// (no predecessor row) also answers "", for a thread root.
//
// Reads only columns migration 0030 fills. A miss must never fall back to "the
// most recent turn": guessing is what put concurrent subagents on one thread
// to begin with.
func (s *CaptureStore) ThreadOfPredecessorInSession(ctx context.Context, session, prevMessageID string) (string, error) {
	if prevMessageID == "" || session == "" {
		return "", nil
	}
	// The LIKE arm must match the session verbatim, so % and _ have to lose
	// their wildcard meaning: a session id is client-supplied (claude.go takes
	// an arbitrary X-Rafiki-Session). escapeLikePattern and the ESCAPE clause
	// are a pair; one backslash in the raw-string SQL literal is load-bearing.
	like := escapeLikePattern(session)
	var threadID string
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(t.thread_id::text, '')
		   FROM conversations.conversation_turn t
		   JOIN conversations.conversation c ON c.id = t.conversation_id
		  WHERE c.driven_by = 'client'
		    AND (c.external_ref = $1 OR c.external_ref LIKE $2 || ':%' ESCAPE '\')
		    AND t.response_message_id = $3
		  ORDER BY t.created_at DESC LIMIT 1`,
		session, like, prevMessageID).Scan(&threadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("thread of predecessor in session: %w", err)
	}
	return threadID, nil
}

// SessionFamilyExists reports whether any conversation of the session's
// family exists yet: the bare "<session>" root row (branches are only ever
// created after it). One indexed lookup on the (external_ref, driven_by)
// unique index, run pre-upstream in the proxy's beginCapture; it is the
// discriminator between the session's MAIN founding turn (family absent:
// bare ref, thread_id NULL) and an INDEPENDENT thread's founding turn
// (family present, no resolvable predecessor, AND the request carries
// cc_is_subagent: a Task subagent; the titler and the quota probe do not
// carry the flag in measured traffic). A probe error is the
// caller's to classify, never a root decision made here.
func (s *CaptureStore) SessionFamilyExists(ctx context.Context, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM conversations.conversation
		    WHERE external_ref = $1 AND driven_by = 'client')`, session).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("session family exists: %w", err)
	}
	return exists, nil
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
	ID             string // optional pre-minted turn id (UUID); empty keeps the DB default (uuidv7())
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
// t.ID, when set, reserves that UUID as the row's id instead of the uuidv7()
// default: the proxy pre-mints a founding turn's id so it can route the
// founding request to the branch named after it before the row exists.
// Ordinal is retained only as an ordering hint, not an update key — intra-turn
// retries and cross-run correlation must never collide on it.
func (s *CaptureStore) InsertTurnIntent(ctx context.Context, t TurnIntent) (turnID string, createdAt time.Time, err error) {
	protocol := t.Protocol
	if protocol == "" {
		protocol = string(store.ProtocolAnthropic)
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation_turn (id, conversation_id, ordinal, status, model, request, source, author_user_id, author_kind, prefix_hash, protocol)
		 VALUES (COALESCE($10::uuid, uuidv7()),$1,$2,'pending',$3,$4,$5,$6::uuid,$7,$8,$9) RETURNING id::text, created_at`,
		t.ConversationID, t.Ordinal, nullify(t.Model), nullifyBytes(jsonbSafe(t.Request)),
		nullify(t.Source), nullUUID(t.AuthorUserID), nullify(t.AuthorKind), nullify(t.PrefixHash), protocol,
		nullUUID(t.ID)).Scan(&turnID, &createdAt)
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

// DecomposeRequest parses reqBody's messages[] into conversation_message rows,
// appended at an ordinal computed from the conversation's resume horizon (see
// resolveHorizon and docs/plans/2026-09-09-claude-compaction-design.md).
// Content is stored verbatim, ON CONFLICT (conversation_id,ordinal) DO
// NOTHING. Also stores prefix_content on this turn iff prefixHash != the
// previous turn's, and records cache-breakpoint ordinals on the turn.
// Best-effort: returns error for the caller to Warn, never panics, never
// alters reqBody. Returns the ordinal the assistant response belongs at
// (horizon + message count) -- NOT a bare message count.
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
	var nextOrdinal int
	err := retryDB(ctx, "decomposeRequest", func(ctx context.Context) error {
		contents := make([]json.RawMessage, len(req.Messages))
		for i, m := range req.Messages {
			contents[i] = m.Content
		}
		horizon, isNewBoundary, herr := s.resolveHorizon(ctx, convID, contents)
		if herr != nil {
			return fmt.Errorf("decompose: resolve horizon: %w", herr)
		}
		for i, m := range req.Messages {
			content := m.Content
			if len(content) == 0 {
				content = json.RawMessage(`null`)
			}
			var kind any
			var inTok *int64
			if i == 0 && isNewBoundary {
				kind = "compaction_summary"
				// The approximate size of the context this boundary replaced:
				// the PREVIOUS turn's input_tokens. This turn's own
				// CompleteTurn already ran earlier in streamAndCapture, so
				// excluding it by created_at is what selects the prior turn
				// rather than this one.
				var prevIn int64
				perr := s.pool.QueryRow(ctx,
					`SELECT coalesce(input_tokens,0) FROM conversations.conversation_turn
					  WHERE conversation_id=$1 AND created_at < $2
					  ORDER BY created_at DESC, id DESC LIMIT 1`,
					convID, createdAt).Scan(&prevIn)
				if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
					return fmt.Errorf("decompose: prior turn input_tokens: %w", perr)
				}
				if prevIn > 0 {
					inTok = &prevIn
				}
			}
			if err := s.appendMessage(ctx, convID, horizon+i, m.Role, jsonbSafe(content), inTok, nil, nil, kind); err != nil {
				return fmt.Errorf("decompose: insert message %d: %w", i, err)
			}
		}
		nextOrdinal = horizon + len(req.Messages)
		return s.StoreTurnPrefix(ctx, convID, turnID, createdAt, reqBody, prefixHash)
	})
	return nextOrdinal, err
}

// resolveHorizon determines the ordinal offset (horizon) this request's
// messages should be inserted at, and whether request message 0 is a NEW
// compaction boundary that must be tagged kind='compaction_summary'. See
// docs/plans/2026-09-09-claude-compaction-design.md §3-4.
//
// Comparison is Postgres JSONB equality (content = $n::jsonb), not a Go-side
// byte or struct compare -- it is whitespace/key-order-insensitive, which a
// re-serialized request is not guaranteed to be.
func (s *CaptureStore) resolveHorizon(ctx context.Context, convID string, messages []json.RawMessage) (horizon int, isNewBoundary bool, err error) {
	var h int
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&h); err != nil {
		return 0, false, fmt.Errorf("resolve horizon: read conversation: %w", err)
	}
	if len(messages) == 0 {
		return h, false, nil
	}
	msg0 := nonEmptyJSON(messages[0])

	var rowExists, matches bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND ordinal=$2),
		        EXISTS(SELECT 1 FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND ordinal=$2 AND content=$3::jsonb)`,
		convID, h, jsonbSafe(msg0)).Scan(&rowExists, &matches); err != nil {
		return 0, false, fmt.Errorf("resolve horizon: read anchor: %w", err)
	}
	if !rowExists || matches {
		// Bootstrap (no anchor row yet -- the conversation's first-ever
		// request) or stable prefix: proceed unchanged. These two cases are
		// deliberately not distinguished; both mean "insert at h, no tag."
		return h, false, nil
	}

	// Divergence. Try the re-anchor guard before recording a new boundary.
	if len(messages) >= 2 {
		reanchored, ok, rerr := s.reanchorHorizon(ctx, convID, msg0, nonEmptyJSON(messages[1]))
		if rerr != nil {
			return 0, false, fmt.Errorf("resolve horizon: re-anchor: %w", rerr)
		}
		if ok {
			return reanchored, false, nil
		}
	}

	// Genuine new boundary. Compute H' and bump the horizon atomically in one
	// transaction (design §4, step 1-2). The marker row itself is inserted by
	// the caller's normal insert loop, NOT in this transaction: if that insert
	// fails and retryDB retries the whole DecomposeRequest call, the next
	// attempt's anchor read finds !rowExists at the (already-bumped) H' and
	// takes the branch above, re-running the insert loop at the correct
	// horizon via the existing ON CONFLICT DO NOTHING idempotency -- at the
	// cost of losing the kind='compaction_summary' tag on that one retry.
	// That degraded outcome is acceptable; a torn write that bumped nothing
	// while a marker row existed would not be, which is why the horizon bump
	// itself is transactional.
	if !looksLikeCompactionSummary(msg0) {
		// Structurally divergent but not a summary: a new thread's preamble, or
		// a client that rewrote its head. Insert at the existing horizon
		// untagged rather than moving the resume point onto it. The divergent
		// head itself is NOT captured (its inserts DO NOTHING against the rows
		// at the horizon) and nothing re-anchors later: the append never wrote
		// it, so reanchorHorizon cannot match it. That is still the safe
		// direction: no false resume point, no readable history lost. The warn
		// is the only signal this happened, so a marker list gone stale shows
		// up in the log instead of only in a manual DB scan.
		slog.Warn("capture: divergent message 0 is not a compaction summary; leaving the horizon untagged",
			"conversation", convID, "head_first_120", string(msg0[:min(len(msg0), 120)]))
		return h, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("resolve horizon: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var hPrime int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(ordinal),-1)+1 FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
		convID).Scan(&hPrime); err != nil {
		return 0, false, fmt.Errorf("resolve horizon: compute H': %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE conversations.conversation SET resume_from_ordinal=$2 WHERE id=$1::uuid`,
		convID, hPrime); err != nil {
		return 0, false, fmt.Errorf("resolve horizon: bump horizon: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("resolve horizon: commit: %w", err)
	}
	return hPrime, true, nil
}

// compactionMarkers are the phrases Claude Code's own compaction summary opens
// with. Structural divergence alone is not enough to tag a boundary: a thread's
// first message is its own session preamble and always diverges, which is how
// 19 preambles came to be tagged compaction_summary while the two real
// summaries in the same database were tagged NULL.
var compactionMarkers = []string{
	"This session is being continued from a previous conversation",
	"ran out of context",
	"The conversation is summarized below",
}

// looksLikeCompactionSummary reports whether a message's content reads as a
// compaction summary rather than a session preamble. Conservative in the safe
// direction: a missed boundary leaves the horizon where it was, the divergent
// head goes uncaptured, and no false resume point is recorded, while a false
// boundary moves the resume point onto an unrelated message and loses the
// history before it.
func looksLikeCompactionSummary(content []byte) bool {
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil || len(blocks) == 0 {
		return false
	}
	for _, m := range compactionMarkers {
		if strings.Contains(blocks[0].Text, m) {
			return true
		}
	}
	return false
}

// reanchorHorizon searches for a positional re-match of the request's head
// (message 0 then message 1 at two consecutive stored ordinals), scoped to
// this conversation's own rows (conversation_id is indexed; no new index is
// added -- this runs only on divergence, at most once per compaction). The
// earliest matching ordinal wins. See design §4 "Guard against pathological
// re-anchoring."
func (s *CaptureStore) reanchorHorizon(ctx context.Context, convID string, msg0, msg1 json.RawMessage) (horizon int, ok bool, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ordinal FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND content=$2::jsonb ORDER BY ordinal`,
		convID, jsonbSafe(msg0))
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var candidates []int
	for rows.Next() {
		var o int
		if err := rows.Scan(&o); err != nil {
			return 0, false, err
		}
		candidates = append(candidates, o)
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	for _, o := range candidates {
		var matches bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND ordinal=$2 AND content=$3::jsonb)`,
			convID, o+1, jsonbSafe(msg1)).Scan(&matches); err != nil {
			return 0, false, err
		}
		if matches {
			return o, true, nil
		}
	}
	return 0, false, nil
}

func nonEmptyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`null`)
	}
	return raw
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
// Unlike DecomposeRequest's request rows, a response that lands on an occupied
// ordinal is never a benign replay: it is a lost reply, so appendMessageStrict
// surfaces it as ErrOrdinalOccupied and the caller fails the turn rather than
// dropping the reply silently. Also sets the turn's response_ordinal to
// `ordinal`. `ordinal` is expected to be DecomposeRequest's return value (the
// horizon-aware ordinal the response belongs at), so the assistant lands right
// after the request's horizon..ordinal-1 messages.
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
		if err := s.appendMessageStrict(ctx, convID, ordinal, "assistant", jsonbSafe(content), &in, &out, nullify(stopReason), nil); err != nil {
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

// RecordThread classifies this turn into its thread and stamps the row:
// response_message_id (the chain key later turns resolve) and thread_id.
//
// Classification, from the two signals the request carries:
//
//   - A resolvable predecessor (diagnostics.previous_message_id resolves to a
//     turn anywhere in the session's family, root row or branch) continues
//     that turn's thread, and its thread_id is copied verbatim: NULL means the
//     main thread, so this turn keeps NULL and stays on the bare session row;
//     a set id means a subagent thread and is inherited.
//   - No resolvable predecessor makes the turn a thread root: thread_id NULL
//     (main thread) when the billing header does not mark it a subagent, its
//     own id (a new thread) when it does. The proxy pre-mints that founding
//     turn's id and routes the founding REQUEST to the branch named after it
//     (beginCapture), so a new thread never shares the root row's ordinal
//     space at all; this method still stamps thread_id = turnID on the row so
//     later turns resolve, and so a founding request whose response was lost
//     (its append failed) keeps its thread identity.
//
// The predecessor lookup is family-scoped (the session's root conversation and
// every "<session>:<threadID>" branch, same shape as
// ThreadOfPredecessorInSession), not scoped to the turn's own conversation: a
// thread's later turns live on a branch while their predecessor's turn lives
// on the root, and a per-conversation lookup would break every chain at that
// boundary and fork a fresh thread per request.
//
// isSubagent is the cc_is_subagent flag parsed from the request's billing
// header (claudethread.BillingFromRequest), decided by the caller so this
// store method never learns to parse Claude Code request bodies. The flag is
// present only when true, so absent means the main thread.
//
// Unresolvable must never fall back to "the most recent turn": that is exactly
// the guess that put concurrent subagents on one thread to begin with.
func (s *CaptureStore) RecordThread(ctx context.Context, session, convID, turnID string, createdAt time.Time, prevMessageID, ownMessageID string, isSubagent bool) error {
	return retryDB(ctx, "recordThread", func(ctx context.Context) error {
		// Main thread by default: thread_id NULL is the convention that keeps
		// the session's root conversation on the bare external_ref.
		threadID := ""
		if isSubagent && prevMessageID == "" {
			// A concurrent subagent's first turn: a new thread root.
			threadID = turnID
		}
		if prevMessageID != "" {
			prevThread, err := s.predecessorThreadID(ctx, session, convID, prevMessageID)
			switch {
			case err == nil:
				// Continue the predecessor's thread, NULL and all.
				threadID = prevThread
			case errors.Is(err, pgx.ErrNoRows):
				// Root: a first turn, a pre-0030 predecessor, or a chain broken
				// by a failed turn that never recorded its id. A subagent here
				// is still a new thread root; the main thread stays NULL.
				if isSubagent {
					threadID = turnID
				}
			default:
				return fmt.Errorf("record thread: resolve predecessor: %w", err)
			}
		}
		tag, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn
			    SET response_message_id = NULLIF($3,''), thread_id = $4::uuid
			  WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, ownMessageID, nullUUID(threadID))
		if err != nil {
			return fmt.Errorf("record thread: update: %w", err)
		}
		if n := tag.RowsAffected(); n != 1 {
			return fmt.Errorf("record thread: expected 1 row, updated %d (stale/skewed key)", n)
		}
		return nil
	})
}

// predecessorThreadID resolves prevMessageID one hop back to the turn that
// produced it and answers that turn's thread_id, "" when the predecessor is
// the main thread. ErrNoRows means the predecessor does not resolve; anything
// else is a database error the caller must not swallow into a root decision.
func (s *CaptureStore) predecessorThreadID(ctx context.Context, session, convID, prevMessageID string) (string, error) {
	if session == "" {
		// No session header: the conversation has no external_ref, so there is
		// no family and the turn's own conversation is the whole scope.
		var prevThread string
		err := s.pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn
			  WHERE conversation_id=$1::uuid AND response_message_id=$2
			  ORDER BY created_at DESC LIMIT 1`,
			convID, prevMessageID).Scan(&prevThread)
		return prevThread, err
	}
	var prevThread string
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(t.thread_id::text, '')
		   FROM conversations.conversation_turn t
		   JOIN conversations.conversation c ON c.id = t.conversation_id
		  WHERE c.driven_by = 'client'
		    AND (c.external_ref = $1 OR c.external_ref LIKE $2 || ':%' ESCAPE '\')
		    AND t.response_message_id = $3
		  ORDER BY t.created_at DESC LIMIT 1`,
		session, escapeLikePattern(session), prevMessageID).Scan(&prevThread)
	return prevThread, err
}

// escapeLikePattern escapes the SQL LIKE wildcards in a client-supplied
// session id so the LIKE arm matches it verbatim. The query's ESCAPE '\'
// clause must agree: one backslash, since in a Go raw string the SQL literal
// is what you see and PostgreSQL rejects any other escape length
// (SQLSTATE 22025).
func escapeLikePattern(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}

// MessageIDFromCanonical reads the assistant message id out of a canonical
// response. Both branches of routing.ParseCapturedResponse carry it: the SSE
// branch marshals an accumulated anthropic.Message, the JSON branch is the
// upstream body verbatim.
func MessageIDFromCanonical(canonical []byte) string {
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(canonical, &m); err != nil {
		return ""
	}
	return m.ID
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
// ErrOrdinalOccupied is likewise deliberately not retryable: a reply that lost
// its ordinal will lose it again.
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
