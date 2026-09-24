// SPDX-License-Identifier: Apache-2.0

// Package recalldb is the Postgres implementation of recall.Store: memory
// CRUD, per-source BM25 and vector search, context expansion, the background
// indexer's working set, summaries, and shared state. Schema comes from
// migration 0036.
package recalldb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/recall"
)

// Store implements recall.Store on Postgres. Memory semantics: a tombstoned
// row (deleted_at IS NOT NULL) is never returned and its live-key slot is
// freed for a fresh Put.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a recall.Store backed by pool, which must point at a database
// migrated to at least version 0036.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ recall.Store = (*Store)(nil)

// sqlb assembles a query while collecting positional bind values; fragments
// appended after earlier args keep their own numbering, so helpers that bind
// (scopeFilter) must be invoked in the same left-to-right order their
// placeholders appear.
type sqlb struct {
	strings.Builder
	args []any
}

// arg binds v and returns its placeholder.
func (b *sqlb) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// scopeFilter returns the owner predicate for scope, binding the owner id.
// The caller must have rejected !scope.Valid() already; alias "" omits the
// column prefix.
func scopeFilter(b *sqlb, scope recall.Scope, alias, col string) string {
	column := col
	if alias != "" {
		column = alias + "." + col
	}
	if scope.All {
		return "TRUE"
	}
	b.args = append(b.args, scope.OwnerUserID)
	return column + " = $" + strconv.Itoa(len(b.args)) + "::uuid"
}

// nullUUID maps "" to SQL NULL for nullable uuid columns.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// kindOf labels a conversation from its origin entrypoint.
func kindOf(originEntrypoint string) string {
	if originEntrypoint == "agent" {
		return "fundi"
	}
	return "claude"
}

// repoBaseSQL renders basename(repo_root) as "" for NULL; the conversation
// table must be aliased c.
const repoBaseSQL = "coalesce(regexp_replace(c.repo_root, '^.*/', ''), '')"

// linkageSQL matches child rows linked to conversation c (Global Constraints,
// with the _/% escapes so c_1:sub cannot match cX1:sub).
const linkageSQL = `ch.conversation_id = c.id
 OR ch.child_id = c.external_ref
 OR c.external_ref LIKE replace(replace(ch.child_id,'_','\_'),'%','\%') || ':%' ESCAPE '\'`

// lastActivitySQL is the newest conversation_message.created_at for c.
const lastActivitySQL = `(SELECT max(m.created_at) FROM conversations.conversation_message m
 WHERE m.conversation_id = c.id)`

// quietIntervalSQL renders recall.QuietPeriod for the stopped/eligibility rules.
var quietIntervalSQL = "interval '" + strconv.FormatInt(int64(recall.QuietPeriod/time.Second), 10) + " seconds'"

// stoppedSQL is the "Stopped" rule verbatim: conversation closed, or a linked
// child exited, or quiet with no live linked child. Evaluates to NULL when
// the conversation has no messages at all; wrap in coalesce where a bool is
// required.
var stoppedSQL = "(c.closed_at IS NOT NULL" +
	" OR EXISTS (SELECT 1 FROM conversations.child ch WHERE (" + linkageSQL + ") AND ch.status = 'exited')" +
	" OR (NOT EXISTS (SELECT 1 FROM conversations.child ch WHERE (" + linkageSQL + ") AND ch.closed_at IS NULL AND ch.status <> 'exited')" +
	" AND " + lastActivitySQL + " < now() - " + quietIntervalSQL + "))"

// pgxRow is the single-row scan surface shared by pgx.Row and pgx.Rows.
type pgxRow interface {
	Scan(dest ...any) error
}

// pgxRows is the multi-row surface of pgx.Rows.
type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// errNoRows maps a single-row scan miss to recall.ErrNotFound exactly.
func errNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return recall.ErrNotFound
	}
	return err
}

// parseID validates a uuid id; anything else is simply not a row we have.
func parseID(id string) (string, bool) {
	if _, err := uuid.Parse(id); err != nil {
		return "", false
	}
	return id, true
}

// --- memories ---

// PutMemory upserts on the live key (the partial unique index), clearing the
// embedding because the body may have changed.
func (s *Store) PutMemory(ctx context.Context, ownerUserID string, m recall.Memory) (recall.Memory, error) {
	if ownerUserID == "" {
		return recall.Memory{}, recall.ErrNoOwner
	}
	if err := recall.ValidPath(m.Path); err != nil {
		return recall.Memory{}, err
	}
	if err := recall.ValidName(m.Name); err != nil {
		return recall.Memory{}, err
	}
	meta := m.Meta
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	var b sqlb
	b.WriteString(`INSERT INTO conversations.memory (owner_user_id, path, name, body, meta)
		VALUES (` + b.arg(ownerUserID) + `::uuid, ` + b.arg(m.Path) + `::ltree, ` + b.arg(m.Name) + `, ` + b.arg(m.Body) + `, ` + b.arg(meta) + `::jsonb)
		ON CONFLICT (owner_user_id, path, name) WHERE deleted_at IS NULL
		DO UPDATE SET body = EXCLUDED.body, meta = EXCLUDED.meta, updated_at = now(),
		              embedding = NULL, embedding_model = NULL
		RETURNING id::text, created_at, updated_at`)
	out := m
	out.OwnerUserID = ownerUserID
	out.Meta = append([]byte(nil), meta...)
	if err := s.pool.QueryRow(ctx, b.String(), b.args...).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return recall.Memory{}, err
	}
	return out, nil
}

// GetMemory reads one live memory.
func (s *Store) GetMemory(ctx context.Context, ownerUserID, path, name string) (recall.Memory, error) {
	if ownerUserID == "" {
		return recall.Memory{}, recall.ErrNoOwner
	}
	if err := recall.ValidPath(path); err != nil {
		return recall.Memory{}, err
	}
	if err := recall.ValidName(name); err != nil {
		return recall.Memory{}, err
	}
	var b sqlb
	b.WriteString(`SELECT id::text, owner_user_id::text, path::text, name, body, meta, created_at, updated_at
		FROM conversations.memory
		WHERE owner_user_id = ` + b.arg(ownerUserID) + `::uuid AND path = ` + b.arg(path) + `::ltree
		  AND name = ` + b.arg(name) + ` AND deleted_at IS NULL`)
	m, err := scanMemory(s.pool.QueryRow(ctx, b.String(), b.args...))
	if err != nil {
		return recall.Memory{}, errNoRows(err)
	}
	return m, nil
}

// MemoryByID reads one live memory by row id.
func (s *Store) MemoryByID(ctx context.Context, ownerUserID, id string) (recall.Memory, error) {
	if ownerUserID == "" {
		return recall.Memory{}, recall.ErrNoOwner
	}
	uid, ok := parseID(id)
	if !ok {
		return recall.Memory{}, recall.ErrNotFound
	}
	var b sqlb
	b.WriteString(`SELECT id::text, owner_user_id::text, path::text, name, body, meta, created_at, updated_at
		FROM conversations.memory
		WHERE owner_user_id = ` + b.arg(ownerUserID) + `::uuid AND id = ` + b.arg(uid) + `::uuid
		  AND deleted_at IS NULL`)
	m, err := scanMemory(s.pool.QueryRow(ctx, b.String(), b.args...))
	if err != nil {
		return recall.Memory{}, errNoRows(err)
	}
	return m, nil
}

// MemoryTree lists live memories under path; depth > 0 bounds how many levels
// below path are included.
func (s *Store) MemoryTree(ctx context.Context, ownerUserID, path string, depth int) ([]recall.Memory, error) {
	if ownerUserID == "" {
		return nil, recall.ErrNoOwner
	}
	if err := recall.ValidPath(path); err != nil {
		return nil, err
	}
	var b sqlb
	b.WriteString(`SELECT id::text, owner_user_id::text, path::text, name, body, meta, created_at, updated_at
		FROM conversations.memory
		WHERE owner_user_id = ` + b.arg(ownerUserID) + `::uuid AND deleted_at IS NULL
		  AND path <@ ` + b.arg(path) + `::ltree`)
	if depth > 0 {
		b.WriteString(` AND nlevel(path) <= nlevel(` + b.arg(path) + `::ltree) + ` + strconv.Itoa(depth))
	}
	b.WriteString(` ORDER BY path, name`)
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMemory tombstones the live row; zero rows affected is not found.
func (s *Store) DeleteMemory(ctx context.Context, ownerUserID, path, name string) error {
	if ownerUserID == "" {
		return recall.ErrNoOwner
	}
	if err := recall.ValidPath(path); err != nil {
		return err
	}
	if err := recall.ValidName(name); err != nil {
		return err
	}
	var b sqlb
	b.WriteString(`UPDATE conversations.memory SET deleted_at = now()
		WHERE owner_user_id = ` + b.arg(ownerUserID) + `::uuid AND path = ` + b.arg(path) + `::ltree
		  AND name = ` + b.arg(name) + ` AND deleted_at IS NULL`)
	tag, err := s.pool.Exec(ctx, b.String(), b.args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return recall.ErrNotFound
	}
	return nil
}

func scanMemory(row pgxRow) (recall.Memory, error) {
	var m recall.Memory
	var meta []byte
	if err := row.Scan(&m.ID, &m.OwnerUserID, &m.Path, &m.Name, &m.Body, &meta, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return recall.Memory{}, err
	}
	m.Meta = append([]byte(nil), meta...)
	return m, nil
}

// --- context expansion ---

// Conversation reads one conversation's metadata under scope.
func (s *Store) Conversation(ctx context.Context, scope recall.Scope, conversationID string) (recall.ConversationMeta, error) {
	if !scope.Valid() {
		return recall.ConversationMeta{}, recall.ErrInvalidScope
	}
	uid, ok := parseID(conversationID)
	if !ok {
		return recall.ConversationMeta{}, recall.ErrNotFound
	}
	var b sqlb
	b.WriteString(`SELECT c.id::text, coalesce(c.owner_user_id::text, ''), coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			c.created_at, c.updated_at,
			(SELECT max(m.ordinal) FROM conversations.conversation_message m WHERE m.conversation_id = c.id),
			` + lastActivitySQL + `,
			coalesce(` + stoppedSQL + `, false)
		FROM conversations.conversation c
		WHERE c.id = ` + b.arg(uid) + `::uuid AND ` + scopeFilter(&b, scope, "c", "owner_user_id"))
	c, err := scanConversationMeta(s.pool.QueryRow(ctx, b.String(), b.args...))
	if err != nil {
		return recall.ConversationMeta{}, errNoRows(err)
	}
	return c, nil
}

// Messages reads a conversation's messages in [fromOrdinal, toOrdinal]; a
// negative toOrdinal is unbounded.
func (s *Store) Messages(ctx context.Context, scope recall.Scope, conversationID string, fromOrdinal, toOrdinal int) ([]recall.Message, error) {
	if !scope.Valid() {
		return nil, recall.ErrInvalidScope
	}
	uid, ok := parseID(conversationID)
	if !ok {
		return nil, recall.ErrNotFound
	}
	var b sqlb
	b.WriteString(`SELECT m.ordinal, m.role, coalesce(m.kind, ''), m.content, m.created_at
		FROM conversations.conversation_message m
		JOIN conversations.conversation c ON c.id = m.conversation_id
		WHERE m.conversation_id = ` + b.arg(uid) + `::uuid AND m.ordinal >= ` + b.arg(fromOrdinal))
	if toOrdinal >= 0 {
		b.WriteString(` AND m.ordinal <= ` + b.arg(toOrdinal))
	}
	b.WriteString(` AND ` + scopeFilter(&b, scope, "c", "owner_user_id") + ` ORDER BY m.ordinal`)
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Window reads one window under scope.
func (s *Store) Window(ctx context.Context, scope recall.Scope, id string) (recall.Window, error) {
	if !scope.Valid() {
		return recall.Window{}, recall.ErrInvalidScope
	}
	uid, ok := parseID(id)
	if !ok {
		return recall.Window{}, recall.ErrNotFound
	}
	var b sqlb
	b.WriteString(`SELECT w.id::text, w.conversation_id::text, coalesce(w.owner_user_id::text, ''), w.seq, w.ordinal_from, w.ordinal_to,
			w.text, w.sealed, w.extractor_version
		FROM conversations.conversation_window w
		JOIN conversations.conversation c ON c.id = w.conversation_id
		WHERE w.id = ` + b.arg(uid) + `::uuid AND ` + scopeFilter(&b, scope, "w", "owner_user_id"))
	var w recall.Window
	var sealed bool
	err := s.pool.QueryRow(ctx, b.String(), b.args...).
		Scan(&w.ID, &w.ConversationID, &w.OwnerUserID, &w.Seq, &w.OrdinalFrom, &w.OrdinalTo, &w.Text, &sealed, &w.ExtractorVersion)
	if err != nil {
		return recall.Window{}, errNoRows(err)
	}
	w.Sealed = sealed
	return w, nil
}

// Summary reads one summary under scope.
func (s *Store) Summary(ctx context.Context, scope recall.Scope, id string) (recall.Summary, error) {
	if !scope.Valid() {
		return recall.Summary{}, recall.ErrInvalidScope
	}
	uid, ok := parseID(id)
	if !ok {
		return recall.Summary{}, recall.ErrNotFound
	}
	var b sqlb
	b.WriteString(`SELECT s.id::text, s.conversation_id::text, coalesce(s.owner_user_id::text, ''), s.level, s.seq, s.ordinal_from, s.ordinal_to,
			s.title, s.summary, s.prompt_version, s.model, s.input_tokens, s.output_tokens, s.total_cost_usd, s.created_at
		FROM conversations.conversation_summary s
		JOIN conversations.conversation c ON c.id = s.conversation_id
		WHERE s.id = ` + b.arg(uid) + `::uuid AND ` + scopeFilter(&b, scope, "s", "owner_user_id"))
	var sm recall.Summary
	err := s.pool.QueryRow(ctx, b.String(), b.args...).
		Scan(&sm.ID, &sm.ConversationID, &sm.OwnerUserID, &sm.Level, &sm.Seq, &sm.OrdinalFrom, &sm.OrdinalTo,
			&sm.Title, &sm.Summary, &sm.PromptVersion, &sm.Model, &sm.InputTokens, &sm.OutputTokens, &sm.TotalCostUSD, &sm.CreatedAt)
	if err != nil {
		return recall.Summary{}, errNoRows(err)
	}
	return sm, nil
}

func scanMessage(row pgxRow) (recall.Message, error) {
	var m recall.Message
	var content []byte
	if err := row.Scan(&m.Ordinal, &m.Role, &m.Kind, &content, &m.CreatedAt); err != nil {
		return recall.Message{}, err
	}
	m.Content = append([]byte(nil), content...)
	return m, nil
}

// scanConversationMeta fills ConversationMeta from the shape Conversation and
// ExtractCursors select.
func scanConversationMeta(row pgxRow) (recall.ConversationMeta, error) {
	var c recall.ConversationMeta
	var repo, entrypoint string
	var maxOrdinal sql.NullInt64
	var lastAt sql.NullTime
	var updatedAt time.Time // selected for ordering, not part of the meta
	if err := row.Scan(&c.ID, &c.OwnerUserID, &c.Name, &repo, &entrypoint, &c.CreatedAt, &updatedAt,
		&maxOrdinal, &lastAt, &c.Stopped); err != nil {
		return recall.ConversationMeta{}, err
	}
	c.Repo = repo
	c.Kind = kindOf(entrypoint)
	c.MaxOrdinal = int(maxOrdinal.Int64)
	c.LastAt = lastAt.Time
	return c, nil
}

// --- state + coordination ---

func (s *Store) GetState(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM conversations.recall_state WHERE key = $1`, key).
		Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO conversations.recall_state (key, value)
		VALUES ($1, $2) ON CONFLICT (key)
		DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

func (s *Store) AddState(ctx context.Context, key string, delta float64) error {
	// Numeric add keeps decimal precision; the stored text round-trips through
	// GetState and Status's ParseFloat.
	_, err := s.pool.Exec(ctx, `INSERT INTO conversations.recall_state (key, value)
		VALUES ($1, (0::numeric + $2::numeric)::text) ON CONFLICT (key)
		DO UPDATE SET value = (coalesce(conversations.recall_state.value, '0')::numeric + $2::numeric)::text,
		              updated_at = now()`, key, delta)
	return err
}

// TryLock holds the recall indexer advisory lock for the worker's life. The
// lock lives on one dedicated pool connection; release unlocks, then returns
// the connection.
func (s *Store) TryLock(ctx context.Context) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('rafiki.recall.indexer'))`).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// The unlock must run even when the caller's ctx is already done.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('rafiki.recall.indexer'))`)
		conn.Release()
	}, true, nil
}

// Status reports index state. EmbeddingModel stays empty: the daemon fills it
// from its configured embedder.
func (s *Store) Status(ctx context.Context) (recall.Status, error) {
	var st recall.Status
	var backfillSince sql.NullString
	err := s.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM conversations.conversation c WHERE c.origin_entrypoint <> ALL($1)),
			(SELECT count(*) FROM conversations.conversation_window),
			(SELECT count(*) FROM conversations.conversation_window WHERE embedding IS NULL),
			(SELECT count(*) FROM conversations.conversation_summary),
			(SELECT count(*) FROM conversations.conversation_summary WHERE embedding IS NULL),
			(SELECT count(*) FROM conversations.memory WHERE deleted_at IS NULL),
			(SELECT coalesce(sum(total_cost_usd), 0) FROM conversations.conversation_summary),
			(SELECT value FROM conversations.recall_state WHERE key = 'backfill_since')`,
		recall.ExcludedEntrypoints,
	).Scan(&st.Conversations, &st.Windows, &st.WindowsUnembedded, &st.Summaries, &st.SummariesPending,
		&st.Memories, &st.SummaryCostUSD, &backfillSince)
	if err != nil {
		return recall.Status{}, err
	}
	if backfillSince.Valid {
		if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(backfillSince.String)); perr == nil {
			st.BackfillSince = &t
		}
	}
	st.BackfillBudgetUSD = stateFloat(ctx, s.pool, "backfill_budget_usd")
	st.BackfillSpentUSD = stateFloat(ctx, s.pool, "backfill_spent_usd")
	return st, nil
}

func stateFloat(ctx context.Context, pool *pgxpool.Pool, key string) float64 {
	var value sql.NullString
	if err := pool.QueryRow(ctx, `SELECT value FROM conversations.recall_state WHERE key = $1`, key).Scan(&value); err != nil || !value.Valid {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value.String), 64)
	if err != nil {
		return 0
	}
	return f
}

// indexHash names a vector index for one (model, dims) pair.
func indexHash(model string, dims int) string {
	sum := sha256.Sum256([]byte(model + ":" + strconv.Itoa(dims)))
	return hex.EncodeToString(sum[:])[:10]
}
