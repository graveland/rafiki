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

	"go.graveland.dev/rafiki/pkg/convtree"
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

// MarkClearPending flags the conversation whose external_ref is externalRef
// (a claude child's main-thread conversation is linked by external_ref =
// child id) so the next predecessor-less request opens a clear root and
// consumes the flag. No such conversation is not an error: a child that has
// captured nothing has nothing to clear.
func (s *CaptureStore) MarkClearPending(ctx context.Context, externalRef string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation SET clear_pending=true WHERE external_ref=$1`,
		externalRef); err != nil {
		return fmt.Errorf("mark clear pending: %w", err)
	}
	return nil
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
	RequestHeadID  string // conversation_message.id of the last path row the request covered; empty → NULL
	TrimFrom       *int   // first path position a trim dropped; nil with TrimTo nil → nothing trimmed
	TrimTo         *int   // one past the last dropped position
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
	if (t.TrimFrom == nil) != (t.TrimTo == nil) {
		return "", time.Time{}, fmt.Errorf("insert turn intent: trim range must set both or neither (from=%v to=%v)", t.TrimFrom != nil, t.TrimTo != nil)
	}
	if t.TrimFrom != nil && *t.TrimFrom > *t.TrimTo {
		return "", time.Time{}, fmt.Errorf("insert turn intent: trim range inverted")
	}
	protocol := t.Protocol
	if protocol == "" {
		protocol = string(store.ProtocolAnthropic)
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation_turn (id, conversation_id, ordinal, status, model, request, source, author_user_id, author_kind, prefix_hash, protocol, request_head_id, request_trim_from, request_trim_to)
		 VALUES (COALESCE($10::uuid, uuidv7()),$1,$2,'pending',$3,$4,$5,$6::uuid,$7,$8,$9,$11::uuid,$12,$13) RETURNING id::text, created_at`,
		t.ConversationID, t.Ordinal, nullify(t.Model), nullifyBytes(jsonbSafe(t.Request)),
		nullify(t.Source), nullUUID(t.AuthorUserID), nullify(t.AuthorKind), nullify(t.PrefixHash), protocol,
		nullUUID(t.ID), nullUUID(t.RequestHeadID), intPtrOrNil(t.TrimFrom), intPtrOrNil(t.TrimTo)).Scan(&turnID, &createdAt)
	if err != nil {
		return "", time.Time{}, err
	}
	// Backfill conversation.model once known: client-driven conversations are
	// created from the session header with no model; the first turn's resolved
	// model fills it. First-seen wins (WHERE model IS NULL) — mid-conversation
	// model changes stay visible per-turn, never rewrite the conversation.
	// Best-effort: a failed backfill must not fail the turn, and the next
	// turn retries it (the column is still NULL). It is logged, not swallowed.
	if t.Model != "" {
		if _, merr := s.pool.Exec(ctx, `UPDATE conversations.conversation
			SET model = $2, updated_at = now()
			WHERE id = $1::uuid AND model IS NULL`, t.ConversationID, t.Model); merr != nil {
			slog.Warn("capture: conversation model backfill failed", "conversation", t.ConversationID, "error", merr)
		}
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
	ServedProvider      string // OpenRouter provider that served the response; empty = not reported (stored NULL)
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
			        model=COALESCE(NULLIF($11,''), model), served_provider=$12
			  WHERE id=$1::uuid AND created_at=$2`,
			r.TurnID, r.CreatedAt, nullifyBytes(jsonbSafe(r.Response)), nullify(r.StopReason), nullify(r.Upstream),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.LatencyMS, r.Model,
			nullify(r.ServedProvider))
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
			if _, merr := s.pool.Exec(ctx, `UPDATE conversations.conversation
				SET model = $3, updated_at = now()
				WHERE id = (
					SELECT conversation_id FROM conversations.conversation_turn
					WHERE id = $1::uuid AND created_at = $2
				) AND model IS NULL`,
				r.TurnID, r.CreatedAt, r.Model); merr != nil {
				slog.Warn("capture: conversation model backfill failed", "turn", r.TurnID, "error", merr)
			}
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

// DecomposeRequest attaches reqBody's messages[] to the conversation's tree and
// records the request's head node on the turn. The returned id is the parent the
// response is appended under.
//
// The attach point comes from the request itself, first match wins:
//
//  1. No diagnostics.previous_message_id: a clear root when a /clear is pending
//     (the flag is consumed in the same transaction as the root), else the rest
//     of the request under an existing root whose content equals message 0 — a
//     retried first request, including the first request after a /clear whose
//     flag the original already consumed, lands on the root it created — else a
//     new plain root.
//  2. The named id resolves to a node of THIS conversation and messages[depth]
//     is the assistant message that node holds: the rest of the request hangs
//     off that node. A message already stored hashes to the node it occupies and
//     is not duplicated; one that differs (the synthetic "No response
//     requested." where a tool_use was stored) becomes a sibling.
//  3. The named id resolves, message 0 reads as a compaction summary and the
//     verification failed: a compaction root derived from that node, carrying
//     the prior turn's input_tokens as the size of the context it replaced.
//  4. Otherwise the request's whole chain is matched by path hash against this
//     conversation's stored nodes and attached at the deepest match, counted as
//     a fallback.
//  5. Nothing matched: a new plain root, derived from the named node when there
//     is one, counted as a fallback.
//
// A parent is always in the same conversation: a hit in another is unknown, so
// a thread's founding request (which names nothing) cannot attach to a sibling
// thread's tree.
//
// Known limit: the hash-chain fallback only finds chains under a plain root with
// no derived-from, because those are the only nodes hashed from
// RootSeed(RootKindRoot, nil). A request whose history starts at a compaction or
// clear root and names no usable id therefore attaches as a new root.
//
// Content is stored verbatim (jsonbSafe'd; empty content becomes JSON null).
// Also stores prefix_content on this turn iff prefixHash differs from the
// previous turn's, and records cache-breakpoints on the turn. Returns the error
// for the caller to escalate (the proxy fails the turn), never panics, never
// alters reqBody.
func (s *CaptureStore) DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash, prevMessageID string) (string, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return "", fmt.Errorf("decompose: unmarshal request: %w", err)
	}
	msgs := make([]convtree.Msg, len(req.Messages))
	for i, m := range req.Messages {
		content := m.Content
		if len(content) == 0 {
			content = json.RawMessage(`null`)
		}
		msgs[i] = convtree.Msg{Role: m.Role, Content: jsonbSafe(content)}
	}
	var head string
	err := retryDB(ctx, "decomposeRequest", func(ctx context.Context) error {
		h, aerr := s.attachRequest(ctx, convID, turnID, createdAt, msgs, prevMessageID)
		if aerr != nil {
			return fmt.Errorf("decompose: %w", aerr)
		}
		head = h
		return s.StoreTurnPrefix(ctx, convID, turnID, createdAt, reqBody, prefixHash)
	})
	if err != nil {
		return "", err
	}
	return head, nil
}

// attachRequest performs the attach decision, appends the chain and records the
// request's head node on the turn. Both writes are idempotent (a chain is
// addressed by its hashes), which is what makes the caller's retry safe.
func (s *CaptureStore) attachRequest(ctx context.Context, convID, turnID string, createdAt time.Time, msgs []convtree.Msg, prevMessageID string) (string, error) {
	if len(msgs) == 0 {
		// A request carrying no history appends nothing: the response hangs off
		// whatever the conversation's head already is.
		var head string
		if err := s.pool.QueryRow(ctx,
			`SELECT coalesce(head_id::text, '') FROM conversations.conversation WHERE id = $1::uuid`,
			convID).Scan(&head); err != nil {
			return "", fmt.Errorf("attach: read head: %w", err)
		}
		if head == "" {
			return "", fmt.Errorf("attach: conversation %s has no messages and no head", convID)
		}
		return head, s.setRequestHead(ctx, turnID, createdAt, head)
	}

	req, err := s.planAttach(ctx, convID, createdAt, msgs, prevMessageID)
	if err != nil {
		return "", err
	}
	res, err := store.AppendChain(ctx, s.pool, s.lease, req)
	if err != nil {
		return "", fmt.Errorf("attach: append chain: %w", err)
	}
	head := req.ParentID
	if n := len(res.Nodes); n > 0 {
		head = res.Nodes[n-1].ID
	}
	if head == "" {
		return "", fmt.Errorf("attach: append produced no head node")
	}
	return head, s.setRequestHead(ctx, turnID, createdAt, head)
}

// planAttach decides where the request hangs and what to append there.
func (s *CaptureStore) planAttach(ctx context.Context, convID string, createdAt time.Time, msgs []convtree.Msg, prevMessageID string) (store.ChainAppend, error) {
	nodes := chainNodes(msgs)
	if prevMessageID == "" {
		return s.planWithoutPredecessor(ctx, convID, nodes, msgs)
	}

	node, found, err := s.nodeByProviderMessageID(ctx, convID, prevMessageID)
	if err != nil {
		return store.ChainAppend{}, err
	}
	reason := "no previous_message_id match"
	if found {
		if d := node.depth; d < len(msgs) && msgs[d].Role == "assistant" && store.SameMessage(node.msg, msgs[d]) {
			return store.ChainAppend{ConversationID: convID, ParentID: node.id, Nodes: nodes[d+1:]}, nil
		}
		if looksLikeCompactionSummary(msgs[0].Content) {
			inTok, ierr := s.priorTurnInputTokens(ctx, convID, createdAt)
			if ierr != nil {
				return store.ChainAppend{}, ierr
			}
			nodes[0].InputTokens = inTok
			return store.ChainAppend{
				ConversationID: convID,
				Root:           convtree.RootKindCompaction,
				DerivedFromID:  node.id,
				Nodes:          nodes,
			}, nil
		}
		reason = "attach message differs"
	}

	parent, depth, ok, err := s.deepestChainMatch(ctx, convID, msgs)
	if err != nil {
		return store.ChainAppend{}, err
	}
	if ok {
		slog.Warn("capture: attached by hash-chain fallback",
			"conversation", convID, "reason", reason, "depth", depth)
		return store.ChainAppend{ConversationID: convID, ParentID: parent, Nodes: nodes[depth+1:], Fallback: true}, nil
	}

	slog.Warn("capture: attached as a new root", "conversation", convID, "reason", reason)
	var derivedFrom string
	if found {
		derivedFrom = node.id
	}
	return store.ChainAppend{
		ConversationID: convID,
		Root:           convtree.RootKindRoot,
		DerivedFromID:  derivedFrom,
		Nodes:          nodes,
		Fallback:       true,
	}, nil
}

// planWithoutPredecessor handles a request that names no predecessor: the
// request itself decides where it starts.
func (s *CaptureStore) planWithoutPredecessor(ctx context.Context, convID string, nodes []store.ChainNode, msgs []convtree.Msg) (store.ChainAppend, error) {
	var clearPending bool
	if err := s.pool.QueryRow(ctx,
		`SELECT clear_pending FROM conversations.conversation WHERE id = $1::uuid`,
		convID).Scan(&clearPending); err != nil {
		return store.ChainAppend{}, fmt.Errorf("attach: read clear_pending: %w", err)
	}
	if clearPending {
		return store.ChainAppend{
			ConversationID:      convID,
			Root:                convtree.RootKindClear,
			DerivedFromHead:     true,
			ConsumeClearPending: true,
			Nodes:               nodes,
		}, nil
	}
	root, ok, err := s.rootMatching(ctx, convID, msgs[0])
	if err != nil {
		return store.ChainAppend{}, err
	}
	if ok {
		return store.ChainAppend{ConversationID: convID, ParentID: root, Nodes: nodes[1:]}, nil
	}
	return store.ChainAppend{ConversationID: convID, Root: convtree.RootKindRoot, Nodes: nodes}, nil
}

// rootMatching returns the id of a root of this conversation that holds the same
// message as m.
func (s *CaptureStore) rootMatching(ctx context.Context, convID string, m convtree.Msg) (string, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, role, content FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND parent_id IS NULL AND path_hash IS NOT NULL
		 ORDER BY ordinal DESC`, convID)
	if err != nil {
		return "", false, fmt.Errorf("attach: read roots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			role    string
			content []byte
		)
		if err := rows.Scan(&id, &role, &content); err != nil {
			return "", false, fmt.Errorf("attach: scan root: %w", err)
		}
		if convtree.Equal(convtree.Msg{Role: role, Content: content}, m) {
			return id, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("attach: read roots: %w", err)
	}
	return "", false, nil
}

// providerNode is a stored node a request named by its provider message id.
type providerNode struct {
	id    string
	depth int
	msg   convtree.Msg
}

// nodeByProviderMessageID resolves diagnostics.previous_message_id to a node of
// this conversation, and only this conversation.
//
// A node without a tree position — a legacy row the backfill has not placed — is
// not an attach point: reporting it unknown lets the hash chain degrade the
// attach instead of failing the turn on a parent AppendChain cannot chain from.
func (s *CaptureStore) nodeByProviderMessageID(ctx context.Context, convID, providerMessageID string) (providerNode, bool, error) {
	var (
		node    providerNode
		content []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, coalesce(depth, 0), role, content
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND provider_message_id = $2 AND path_hash IS NOT NULL
		 ORDER BY ordinal DESC LIMIT 1`,
		convID, providerMessageID).Scan(&node.id, &node.depth, &node.msg.Role, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return providerNode{}, false, nil
	}
	if err != nil {
		return providerNode{}, false, fmt.Errorf("attach: resolve previous_message_id: %w", err)
	}
	node.msg.Content = content
	return node, true, nil
}

// deepestChainMatch hashes the request's whole chain from a plain root and finds
// the deepest node of this conversation whose stored path hash it matches,
// returning that node's id and its index in msgs.
func (s *CaptureStore) deepestChainMatch(ctx context.Context, convID string, msgs []convtree.Msg) (string, int, bool, error) {
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil), msgs)
	if err != nil {
		return "", 0, false, fmt.Errorf("attach: hash chain: %w", err)
	}
	raw := make([][]byte, len(hashes))
	for i, h := range hashes {
		raw[i] = h[:]
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, path_hash FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND path_hash = ANY($2::bytea[])`, convID, raw)
	if err != nil {
		return "", 0, false, fmt.Errorf("attach: match chain: %w", err)
	}
	defer rows.Close()
	byHash := make(map[string]string, len(hashes))
	for rows.Next() {
		var (
			nodeID string
			key    []byte
		)
		if err := rows.Scan(&nodeID, &key); err != nil {
			return "", 0, false, fmt.Errorf("attach: scan chain match: %w", err)
		}
		byHash[string(key)] = nodeID
	}
	if err := rows.Err(); err != nil {
		return "", 0, false, fmt.Errorf("attach: match chain: %w", err)
	}
	for i := len(hashes) - 1; i >= 0; i-- {
		if nodeID, hit := byHash[string(hashes[i][:])]; hit {
			return nodeID, i, true, nil
		}
	}
	return "", 0, false, nil
}

// priorTurnInputTokens is the approximate size of the context a compaction
// summary replaced: the PREVIOUS turn's input_tokens. This turn's own
// CompleteTurn already ran earlier in streamAndCapture, so excluding it by
// created_at is what selects the prior turn rather than this one. A /clear
// carries no size, so it never asks.
func (s *CaptureStore) priorTurnInputTokens(ctx context.Context, convID string, createdAt time.Time) (*int64, error) {
	var prevIn int64
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(input_tokens,0) FROM conversations.conversation_turn
		  WHERE conversation_id=$1 AND created_at < $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		convID, createdAt).Scan(&prevIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("attach: prior turn input_tokens: %w", err)
	}
	if prevIn <= 0 {
		return nil, nil
	}
	return &prevIn, nil
}

// chainNodes renders parsed messages as chain nodes.
func chainNodes(msgs []convtree.Msg) []store.ChainNode {
	nodes := make([]store.ChainNode, len(msgs))
	for i, m := range msgs {
		nodes[i] = store.ChainNode{Role: m.Role, Content: m.Content}
	}
	return nodes
}

// setRequestHead records the request's head node on the turn.
func (s *CaptureStore) setRequestHead(ctx context.Context, turnID string, createdAt time.Time, head string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.conversation_turn SET request_head_id = $3::uuid WHERE id = $1::uuid AND created_at = $2`,
		turnID, createdAt, head)
	if err != nil {
		return fmt.Errorf("set request head: %w", err)
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("set request head: expected 1 row, updated %d (stale/skewed key)", n)
	}
	return nil
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
	head := headText(content)
	if head == "" {
		return false
	}
	for _, m := range compactionMarkers {
		if strings.Contains(head, m) {
			return true
		}
	}
	return false
}

// headText returns the leading text of a message's content, accepting BOTH
// shapes the Anthropic API allows for `content`: a bare JSON string, and an
// array of content blocks (first block's text). Claude Code sends its
// compaction summary as the bare-string form, so an array-only decode read a
// real boundary as "not a summary" and left the summary attached as an ordinary
// message instead of opening the root the history continues from.
func headText(content []byte) string {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return ""
		}
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil || len(blocks) == 0 {
		return ""
	}
	return blocks[0].Text
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
// anthropic.Message JSON) as a child of `parentID` — the request head
// DecomposeRequest returned — with content stored verbatim from
// canonical.content, plus token usage, stop_reason and the response's provider
// message id. Also sets the turn's response_ordinal to the node's ordinal; a
// turn row that cannot be found to point at the reply fails the turn rather than
// leaving the reply unaccounted for.
//
// The tree makes the old ordinal collision impossible to lose a reply to: the
// same reply appended twice is the same node (no new row, head still moves to
// it), and a different reply to the same history is a sibling, not an error. A
// missing parent is a real error and fails the turn: a reply with no request
// head above it would be an orphan.
func (s *CaptureStore) AppendResponseMessage(ctx context.Context, convID, turnID string, createdAt time.Time, parentID string, canonical []byte, in, out int64, stopReason string) error {
	if parentID == "" {
		return fmt.Errorf("append response: no request head to append under (conversation %s)", convID)
	}
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
		res, err := store.AppendChain(ctx, s.pool, s.lease, store.ChainAppend{
			ConversationID: convID,
			ParentID:       parentID,
			Nodes: []store.ChainNode{{
				Role:              "assistant",
				Content:           jsonbSafe(content),
				InputTokens:       &in,
				OutputTokens:      &out,
				StopReason:        stopReason,
				ProviderMessageID: MessageIDFromCanonical(canonical),
			}},
		})
		if err != nil {
			return fmt.Errorf("append response: %w", err)
		}
		if len(res.Nodes) != 1 {
			return fmt.Errorf("append response: expected 1 node, got %d", len(res.Nodes))
		}
		tag, err := s.pool.Exec(ctx,
			`UPDATE conversations.conversation_turn SET response_ordinal=$3 WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt, res.Nodes[0].Ordinal)
		if err != nil {
			return fmt.Errorf("append response: set response_ordinal: %w", err)
		}
		// Zero rows means the turn row is gone or its key moved (id + created_at is
		// the hypertable's primary key), so the reply is stored with no pointer back
		// to it from the turn. Fail the turn rather than complete it half-recorded;
		// setRequestHead checks the same thing for the request side.
		if n := tag.RowsAffected(); n != 1 {
			return fmt.Errorf("append response: set response_ordinal: expected 1 row, updated %d (turn row missing or its key moved)", n)
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

// intPtrOrNil maps a nil *int to nil so pgx writes SQL NULL rather than a zero
// for an unset integer column.
func intPtrOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
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
// A lost lease is likewise deliberately not retryable: it is an answer, not a
// blip.
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
