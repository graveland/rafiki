package recalldb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/recall"
)

// BM25 index names in the form to_bm25query requires: SCHEMA-QUALIFIED. The
// bare name fails with "index ... does not exist" unless the conversations
// schema is on search_path; the qualified name works under both plans
// (verified by EXPLAIN against a scratch database).
const (
	bm25IndexMemory  = "conversations.memory_bm25"
	bm25IndexWindow  = "conversations.conversation_window_bm25"
	bm25IndexSummary = "conversations.conversation_summary_bm25"

	// summarySearchExp is the exact expression conversation_summary_bm25 is
	// built on; the search predicate must match it character for character.
	summarySearchExp = "(s.title || ' ' || s.summary)"
)

// quoteLiteral renders a string as a single-quoted SQL literal
// (pgx.Identifier-style escaping). Models are config values, but escaped
// anyway.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// convFilters appends the shared conversation-derived filters (repo basename,
// since, until). The conversation table must be aliased c.
func convFilters(b *sqlb, q recall.SearchQuery, whenCol string) {
	if q.Repo != "" {
		b.WriteString(` AND regexp_replace(c.repo_root, '^.*/', '') = ` + b.arg(q.Repo))
	}
	if q.Since != nil {
		b.WriteString(` AND ` + whenCol + ` >= ` + b.arg(*q.Since))
	}
	if q.Until != nil {
		b.WriteString(` AND ` + whenCol + ` <= ` + b.arg(*q.Until))
	}
}

// memoryFilters appends the memory-source filters (under path, since, until).
func memoryFilters(b *sqlb, q recall.SearchQuery) {
	if q.Under != "" {
		b.WriteString(` AND m.path <@ ` + b.arg(q.Under) + `::ltree`)
	}
	if q.Since != nil {
		b.WriteString(` AND m.updated_at >= ` + b.arg(*q.Since))
	}
	if q.Until != nil {
		b.WriteString(` AND m.updated_at <= ` + b.arg(*q.Until))
	}
}

// hitLimit clamps the per-source fetch size.
func hitLimit(q recall.SearchQuery) int {
	if q.Limit <= 0 {
		return recall.RecallDefaultLimit
	}
	return q.Limit
}

// SearchBM25 returns one BM25-ranked hit list for src. to_bm25query always
// gets the named index because WHERE filters are always present (the score
// match predicate below) — the named form is what keeps the planner on the
// bm25 index there. The score operator is this extension's negated BM25
// score: matches are strictly negative, non-matches exactly 0, so ASC order
// plus `< 0` yields best matches first.
func (s *Store) SearchBM25(ctx context.Context, q recall.SearchQuery, src recall.Source) ([]recall.Hit, error) {
	if src != recall.SourceMemory && !q.Scope.Valid() {
		return nil, recall.ErrInvalidScope
	}
	if q.Text == "" {
		return nil, nil
	}
	limit := hitLimit(q)
	switch src {
	case recall.SourceMemory:
		return s.searchBM25Memory(ctx, q, limit)
	case recall.SourceSummary:
		return s.searchBM25Summary(ctx, q, limit)
	case recall.SourceWindow:
		return s.searchBM25Window(ctx, q, limit)
	}
	return nil, fmt.Errorf("recalldb: unknown source %q", src)
}

func (s *Store) searchBM25Window(ctx context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error) {
	var b sqlb
	queryExpr := "w.text <@> to_bm25query(" + b.arg(q.Text) + ", '" + bm25IndexWindow + "')"
	b.WriteString(`SELECT w.id::text, w.conversation_id::text, coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			w.ordinal_from, w.ordinal_to, w.created_at, w.text
		FROM conversations.conversation_window w
		JOIN conversations.conversation c ON c.id = w.conversation_id
		WHERE ` + queryExpr + ` < 0
		  AND ` + scopeFilter(&b, q.Scope, "w", "owner_user_id"))
	convFilters(&b, q, "w.created_at")
	b.WriteString(` ORDER BY ` + queryExpr + ` LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanWindowHits(rows, q.Text)
}

func (s *Store) searchBM25Summary(ctx context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error) {
	var b sqlb
	queryExpr := summarySearchExp + " <@> to_bm25query(" + b.arg(q.Text) + ", '" + bm25IndexSummary + "')"
	b.WriteString(`SELECT s.id::text, s.conversation_id::text, coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			s.ordinal_from, s.ordinal_to, s.created_at, s.title, s.summary
		FROM conversations.conversation_summary s
		JOIN conversations.conversation c ON c.id = s.conversation_id
		WHERE ` + queryExpr + ` < 0
		  AND (s.level = 'conversation' OR s.level = 'segment')
		  AND ` + scopeFilter(&b, q.Scope, "s", "owner_user_id"))
	convFilters(&b, q, "s.created_at")
	b.WriteString(` ORDER BY ` + queryExpr + ` LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanSummaryHits(rows, q.Text)
}

func (s *Store) searchBM25Memory(ctx context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error) {
	if q.MemoryOwner == "" {
		return nil, nil
	}
	var b sqlb
	queryExpr := "m.body <@> to_bm25query(" + b.arg(q.Text) + ", '" + bm25IndexMemory + "')"
	b.WriteString(`SELECT m.id::text, m.path::text, m.name, m.updated_at, m.body
		FROM conversations.memory m
		WHERE m.owner_user_id = ` + b.arg(q.MemoryOwner) + `::uuid AND m.deleted_at IS NULL
		  AND ` + queryExpr + ` < 0`)
	memoryFilters(&b, q)
	b.WriteString(` ORDER BY ` + queryExpr + ` LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanMemoryHits(rows, q.Text)
}

// SearchVector returns one embedding-distance-ranked list for src. Only rows
// whose embedding_model equals model are eligible, and the model is written
// as an escaped SQL LITERAL — never a bind parameter: the partial HNSW
// index's predicate can only be proven implied by a literal, and a generic
// prepared plan would silently seq-scan (pinned by
// TestStoreSearchVectorUsesPartialIndex). dims is a literal too, because
// typmods cannot be bound; vec travels as a bind param in pgvector text
// format. Snippet is the head of the text (no term to centre on).
func (s *Store) SearchVector(ctx context.Context, q recall.SearchQuery, src recall.Source, model string, vec []float32) ([]recall.Hit, error) {
	if src != recall.SourceMemory && !q.Scope.Valid() {
		return nil, recall.ErrInvalidScope
	}
	if q.Text == "" || len(vec) == 0 {
		return nil, nil
	}
	limit := hitLimit(q)
	switch src {
	case recall.SourceMemory:
		return s.searchVectorMemory(ctx, q, model, vec, limit)
	case recall.SourceSummary:
		return s.searchVectorSummary(ctx, q, model, vec, limit)
	case recall.SourceWindow:
		return s.searchVectorWindow(ctx, q, model, vec, limit)
	}
	return nil, fmt.Errorf("recalldb: unknown source %q", src)
}

// vecDistance builds the ORDER BY expression for alias: an exact match of the
// HNSW index expression (embedding::vector(<dims>)) with the vec bound in
// pgvector text format.
func vecDistance(b *sqlb, alias, dims string, vec []float32) string {
	return fmt.Sprintf("(%s.embedding::vector(%s)) <=> %s::vector(%s)",
		alias, dims, b.arg(vecText(vec)), dims)
}

func (s *Store) searchVectorWindow(ctx context.Context, q recall.SearchQuery, model string, vec []float32, limit int) ([]recall.Hit, error) {
	sql, args := searchVectorWindowSQL(q, model, vec, limit)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanWindowHits(rows, "")
}

// searchVectorWindowSQL builds the window vector query; factored out so the
// partial-index test can EXPLAIN the exact SQL the store runs.
func searchVectorWindowSQL(q recall.SearchQuery, model string, vec []float32, limit int) (string, []any) {
	var b sqlb
	dims := strconv.Itoa(len(vec))
	b.WriteString(`SELECT w.id::text, w.conversation_id::text, coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			w.ordinal_from, w.ordinal_to, w.created_at, w.text
		FROM conversations.conversation_window w
		JOIN conversations.conversation c ON c.id = w.conversation_id
		WHERE w.embedding_model = ` + quoteLiteral(model) + `
		  AND w.embedding IS NOT NULL
		  AND ` + scopeFilter(&b, q.Scope, "w", "owner_user_id"))
	convFilters(&b, q, "w.created_at")
	b.WriteString(` ORDER BY ` + vecDistance(&b, "w", dims, vec) + ` LIMIT ` + strconv.Itoa(limit))
	return b.String(), b.args
}

func (s *Store) searchVectorSummary(ctx context.Context, q recall.SearchQuery, model string, vec []float32, limit int) ([]recall.Hit, error) {
	var b sqlb
	dims := strconv.Itoa(len(vec))
	b.WriteString(`SELECT s.id::text, s.conversation_id::text, coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			s.ordinal_from, s.ordinal_to, s.created_at, s.title, s.summary
		FROM conversations.conversation_summary s
		JOIN conversations.conversation c ON c.id = s.conversation_id
		WHERE s.embedding_model = ` + quoteLiteral(model) + `
		  AND s.embedding IS NOT NULL
		  AND (s.level = 'conversation' OR s.level = 'segment')
		  AND ` + scopeFilter(&b, q.Scope, "s", "owner_user_id"))
	convFilters(&b, q, "s.created_at")
	b.WriteString(` ORDER BY ` + vecDistance(&b, "s", dims, vec) + ` LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanSummaryHits(rows, "")
}

func (s *Store) searchVectorMemory(ctx context.Context, q recall.SearchQuery, model string, vec []float32, limit int) ([]recall.Hit, error) {
	if q.MemoryOwner == "" {
		return nil, nil
	}
	var b sqlb
	dims := strconv.Itoa(len(vec))
	b.WriteString(`SELECT m.id::text, m.path::text, m.name, m.updated_at, m.body
		FROM conversations.memory m
		WHERE m.embedding_model = ` + quoteLiteral(model) + `
		  AND m.embedding IS NOT NULL
		  AND m.deleted_at IS NULL
		  AND m.owner_user_id = ` + b.arg(q.MemoryOwner) + `::uuid`)
	memoryFilters(&b, q)
	b.WriteString(` ORDER BY ` + vecDistance(&b, "m", dims, vec) + ` LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanMemoryHits(rows, "")
}

func scanWindowHits(rows pgxRows, text string) ([]recall.Hit, error) {
	defer rows.Close()
	var out []recall.Hit
	for i := 0; rows.Next(); i++ {
		var h recall.Hit
		var id, repo, entrypoint, body string
		if err := rows.Scan(&id, &h.ConversationID, &h.ConversationName, &repo, &entrypoint,
			&h.OrdinalFrom, &h.OrdinalTo, &h.When, &body); err != nil {
			return nil, err
		}
		h.Source = recall.SourceWindow
		h.ID = recall.HitID(recall.SourceWindow, id)
		h.Repo = repo
		h.Kind = kindOf(entrypoint)
		h.Snippet = recall.Snippet(body, text)
		h.Rank = i + 1
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanSummaryHits(rows pgxRows, text string) ([]recall.Hit, error) {
	defer rows.Close()
	var out []recall.Hit
	for i := 0; rows.Next(); i++ {
		var h recall.Hit
		var id, repo, entrypoint, title, summary string
		if err := rows.Scan(&id, &h.ConversationID, &h.ConversationName, &repo, &entrypoint,
			&h.OrdinalFrom, &h.OrdinalTo, &h.When, &title, &summary); err != nil {
			return nil, err
		}
		h.Source = recall.SourceSummary
		h.ID = recall.HitID(recall.SourceSummary, id)
		h.Repo = repo
		h.Kind = kindOf(entrypoint)
		h.Title = title
		h.Snippet = recall.Snippet(summary, text)
		h.Rank = i + 1
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanMemoryHits(rows pgxRows, text string) ([]recall.Hit, error) {
	defer rows.Close()
	var out []recall.Hit
	for i := 0; rows.Next(); i++ {
		var h recall.Hit
		var id, path, name, body string
		if err := rows.Scan(&id, &path, &name, &h.When, &body); err != nil {
			return nil, err
		}
		h.Source = recall.SourceMemory
		h.ID = recall.HitID(recall.SourceMemory, id)
		h.Path = path
		h.Name = name
		h.Snippet = recall.Snippet(body, text)
		h.Rank = i + 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// EnsureVectorIndexes creates the three partial HNSW indexes for one
// (model, dims) pair. Statements run outside a transaction (CONCURRENTLY
// requires it) and are idempotent: the name hashes model and dims.
func (s *Store) EnsureVectorIndexes(ctx context.Context, model string, dims int) error {
	if dims <= 0 {
		return fmt.Errorf("recalldb: vector dims must be positive, got %d", dims)
	}
	name := indexHash(model, dims)
	for _, spec := range []struct{ table, prefix string }{
		{"memory", "recall_memory_emb_"},
		{"conversation_window", "recall_window_emb_"},
		{"conversation_summary", "recall_summary_emb_"},
	} {
		sql := fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s%s ON conversations.%s
			USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
			WHERE embedding_model = %s`,
			spec.prefix, name, spec.table, dims, quoteLiteral(model))
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("recalldb: ensure %s%s: %w", spec.prefix, name, err)
		}
	}
	return nil
}

// vecText renders vec in pgvector's text input format: '[1,2,3]'. Only
// strconv-formatted floats reach the string, so there is no injection
// surface.
func vecText(vec []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
