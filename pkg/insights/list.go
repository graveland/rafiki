// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ListSort selects the row order of ListConversations.
type ListSort int

const (
	ListSortRecent    ListSort = iota // updated_at DESC, id DESC (zero value = default)
	ListSortCreated                   // created_at DESC, id DESC
	ListSortRelevance                 // best BM25 score first; requires Query
)

// ListState is the lifecycle state of the agent linked to a conversation.
type ListState string

const (
	ListStateOpen    ListState = "open"     // linked child has exited_at and closed_at unset
	ListStateExited  ListState = "exited"   // linked child has exited_at set, closed_at unset
	ListStateClosed  ListState = "closed"   // linked child has closed_at set
	ListStateNoAgent ListState = "no_agent" // no child links to the conversation
)

// ListFilter narrows a conversation listing. Zero-value fields are ignored, so
// an empty filter lists the top level of the scope, most recent first.
type ListFilter struct {
	Limit                int               // <= 0 → 50; clamped to 500
	Cursor               string            // opaque next value from the previous page
	Sort                 ListSort          // ListSortRecent when zero
	ParentConversationID string            // "" = top level (parent IS NULL)
	Query                string            // BM25 match over conversation_window text
	States               []ListState       // empty = all
	Kinds                []string          // empty = all; a non-empty list excludes no-agent rows
	NameContains         string            // ILIKE on conversation name OR child name
	CwdContains          string            // ILIKE on conversation cwd OR child cwd
	Labels               map[string]string // child labels @> jsonb
	Since, Until         *time.Time        // bounds on updated_at
	Excluded             []string          // origin_entrypoint values never listed; caller passes recall.ExcludedEntrypoints
}

// ConversationListing is one row of ListConversations: the conversation, its
// linked child (when one exists) and the aggregates the page needs.
type ConversationListing struct {
	ID    string
	Name  string
	Model string
	Owner string // username, resolved through the users FK

	CreatedAt    time.Time
	LastActivity time.Time // conversation.updated_at
	ClosedAt     *time.Time

	Turns   int
	CostUSD float64

	ParentConversationID string // "" when the row is top level
	ChildCount           int    // members of the visible set whose parent is this row

	ChildID   string // "" when no child links to the conversation
	ChildName string
	Kind      string
	State     ListState
}

// ErrBadCursor is returned when f.Cursor cannot serve this request: it does not
// decode, or its sort or parent differs from the request's.
var ErrBadCursor = errors.New("insights: cursor does not match this request")

// ErrRelevanceNeedsQuery is returned when ListSortRelevance is requested
// without a Query: there is nothing to score against.
var ErrRelevanceNeedsQuery = errors.New("insights: relevance sort requires a query")

// ErrContentSearchUnavailable is returned when a Query is requested on a
// database whose conversation_window_bm25 index is missing (recall search is
// off there).
var ErrContentSearchUnavailable = errors.New("insights: content search is unavailable on this database")

const (
	defaultListLimit = 50
	maxListLimit     = 500

	// bm25WindowIndex is the index name handed to to_bm25query. It must be a
	// literal: the function resolves the index at plan time.
	bm25WindowIndex = "conversations.conversation_window_bm25"
)

// normalizeListSort maps any ListSort other than the named constants to
// ListSortRecent, so a stray value behaves like the zero value.
func normalizeListSort(s ListSort) ListSort {
	switch s {
	case ListSortCreated, ListSortRelevance:
		return s
	default:
		return ListSortRecent
	}
}

// strictUUID accepts the canonical hyphenated form in any case: the input is
// lowercased before comparing against the parsed rendering, so an
// upper-case-spelled uuid a human pastes is accepted, not silently treated as
// malformed. uuid.Parse also takes urn-prefixed, braced and hyphenless
// spellings that Postgres' ::uuid cast refuses, so a value admitted by the
// parser alone would reach SQL and fail with SQLSTATE 22P02 instead of the
// malformed answer; the comparison refuses those.
func strictUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == strings.ToLower(s)
}

// ilikePattern renders v as an ILIKE pattern matching v literally: the three
// pattern metacharacters are escaped, and every condition using the pattern
// writes ESCAPE '\' so the match does not depend on the server's escape
// default.
func ilikePattern(v string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range v {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

// ListConversations lists one level of the conversation tree under scope.
// The visible set is the matched set (every eligible conversation satisfying
// f) plus every ancestor of a matched conversation, so a filter that hits a
// grandchild still lists the grandfather path down to it. Rows returned are
// the visible members whose parent is f.ParentConversationID ("", top level);
// f.Cursor continues after the previous page's last row. The returned next
// value is "" when the page is the last one.
//
// Turns and CostUSD are computed for the returned page only. ChildCount is the
// number of visible members whose parent is the row, so it always equals what
// an expansion with the same filter returns.
func (i *Insights) ListConversations(ctx context.Context, scope Scope, f ListFilter) ([]ConversationListing, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	} else if limit > maxListLimit {
		limit = maxListLimit
	}
	sort := normalizeListSort(f.Sort)
	if sort == ListSortRelevance && f.Query == "" {
		return nil, "", ErrRelevanceNeedsQuery
	}

	// A malformed parent matches nothing, the same answer an unknown
	// well-formed parent gives, so callers cannot distinguish the two the
	// same way Visible cannot. The check is strict: spellings uuid.Parse
	// tolerates but Postgres' ::uuid cast refuses must land here too, not in
	// a SQL error. A canonical uuid spelled in either case names one parent,
	// so the id is lower-cased once, here: the cursor carries that spelling
	// and the page clause binds it, and a continuation that passes the id
	// this listing reports (Postgres renders uuids in lower case) must be
	// accepted.
	parent := ""
	if f.ParentConversationID != "" {
		if !strictUUID(f.ParentConversationID) {
			return nil, "", nil
		}
		parent = strings.ToLower(f.ParentConversationID)
	}

	var cursorID string
	var cursorAt *time.Time
	var cursorScore *float64
	if f.Cursor != "" {
		cs, err := decodeListCursor(f.Cursor, sort, parent)
		if err != nil {
			return nil, "", err
		}
		cursorID = cs.ID
		cursorAt = cs.T
		cursorScore = cs.F
	}

	haveQuery := f.Query != ""
	if haveQuery {
		ok, err := i.bm25WindowAvailable(ctx)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", ErrContentSearchUnavailable
		}
	}

	// Placeholders map to bind args by allocation order, not by their
	// position in the SQL text: each clause below allocates its own
	// arguments as the builder reaches it.
	var a argList
	scopeCond := scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref")
	excluded := f.Excluded
	if excluded == nil {
		excluded = []string{}
	}
	exclArg := a.next(excluded)

	elig := `
	SELECT c.id AS id,
	       coalesce(c.name, '') AS name,
	       coalesce(u.username, '') AS owner,
	       coalesce(c.model, '') AS model,
	       c.cwd AS cwd,
	       c.created_at AS created_at,
	       c.updated_at AS updated_at,
	       c.closed_at AS closed_at,
	       c.parent_conversation_id AS parent_id,
	       coalesce(k1.child_id, k2.child_id) AS child_id,
	       coalesce(k1.kind, k2.kind) AS child_kind,
	       coalesce(k1.name, k2.name) AS child_name,
	       coalesce(k1.cwd, k2.cwd) AS child_cwd,
	       coalesce(k1.labels, k2.labels) AS child_labels,
	       coalesce(k1.closed_at, k2.closed_at) AS child_closed_at,
	       coalesce(k1.exited_at, k2.exited_at) AS child_exited_at,
	       CASE
	           WHEN coalesce(k1.child_id, k2.child_id) IS NULL THEN '` + string(ListStateNoAgent) + `'
	           WHEN coalesce(k1.closed_at, k2.closed_at) IS NOT NULL THEN '` + string(ListStateClosed) + `'
	           WHEN coalesce(k1.exited_at, k2.exited_at) IS NOT NULL THEN '` + string(ListStateExited) + `'
	           ELSE '` + string(ListStateOpen) + `'
	       END AS state
	FROM conversations.conversation c
	LEFT JOIN conversations.users u ON u.id = c.owner_user_id
	-- child_conversation_idx is not unique: two child rows can share one
	-- conversation, and a plain join would duplicate the row, double its
	-- ChildCount and double its listing. Take one child deterministically,
	-- the lowest child_id.
	LEFT JOIN LATERAL (
	    SELECT ch.* FROM conversations.child ch
	    WHERE ch.conversation_id = c.id
	    ORDER BY ch.child_id
	    LIMIT 1
	) k1 ON true
	LEFT JOIN conversations.child k2 ON k2.child_id = split_part(c.external_ref, ':', 1)
	WHERE ` + scopeCond + `
	  AND coalesce(c.origin_entrypoint, '') <> ALL(` + exclArg + `::text[])
	  AND (c.node_count > 0 OR c.fork_from IS NOT NULL)`

	var mConds []string
	if len(f.States) > 0 {
		states := make([]string, len(f.States))
		for n, s := range f.States {
			states[n] = string(s)
		}
		mConds = append(mConds, "e.state = ANY("+a.next(states)+")")
	}
	if len(f.Kinds) > 0 {
		// No-agent rows carry a NULL child_kind, which never matches ANY, so a
		// non-empty kind list excludes them without a separate clause.
		mConds = append(mConds, "e.child_kind = ANY("+a.next(f.Kinds)+")")
	}
	if f.NameContains != "" {
		pat := a.next(ilikePattern(f.NameContains))
		mConds = append(mConds, "(e.name ILIKE "+pat+" ESCAPE '\\' OR coalesce(e.child_name, '') ILIKE "+pat+" ESCAPE '\\')")
	}
	if f.CwdContains != "" {
		pat := a.next(ilikePattern(f.CwdContains))
		mConds = append(mConds, "(coalesce(e.cwd, '') ILIKE "+pat+" ESCAPE '\\' OR coalesce(e.child_cwd, '') ILIKE "+pat+" ESCAPE '\\')")
	}
	if len(f.Labels) > 0 {
		b, err := json.Marshal(f.Labels)
		if err != nil {
			return nil, "", fmt.Errorf("encode labels filter: %w", err)
		}
		// A child's labels column is NOT NULL, and a no-agent row has no
		// labels at all, so the containment also drops childless rows.
		mConds = append(mConds, "e.child_labels @> "+a.next(string(b))+"::jsonb")
	}
	if f.Since != nil {
		mConds = append(mConds, "e.updated_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		mConds = append(mConds, "e.updated_at < "+a.next(*f.Until))
	}

	// The score of a matched conversation is the min BM25 expression over its
	// windows: negative for a match, so score < 0 is the match test and the
	// same expression feeds the relevance order.
	matched := "SELECT e.*"
	if haveQuery {
		qArg := a.next(f.Query)
		matched += ", wm.score"
		matched += `
	FROM elig e
	CROSS JOIN LATERAL (
	    SELECT min(w.text <@> to_bm25query(` + qArg + ", '" + bm25WindowIndex + `')) AS score
	    FROM conversations.conversation_window w
	    WHERE w.conversation_id = e.id
	) wm`
	} else {
		matched += "\nFROM elig e"
	}
	if haveQuery {
		mConds = append([]string{"wm.score < 0"}, mConds...)
	}
	if len(mConds) > 0 {
		matched += "\nWHERE " + strings.Join(mConds, "\n  AND ")
	}

	// The visible set: matched rows plus every ancestor that is itself
	// eligible. With relevance the walk also carries each matched row's score
	// up the parent chain so an ancestor-only row takes the best score among
	// its matched descendants.
	var visible string
	if sort == ListSortRelevance {
		visible = `,
	walk AS (
	    SELECT m.id AS node, m.parent_id AS up, m.id AS origin, m.score AS score
	    FROM matched m
	    UNION
	    SELECT e.id, e.parent_id, walk.origin, walk.score
	    FROM elig e JOIN walk ON e.id = walk.up
	),
	vrow AS (
	    SELECT e.*,
	           CASE WHEN m.id IS NOT NULL THEN m.score
	                ELSE (SELECT min(walk.score) FROM walk
	                       WHERE walk.node = v.node AND walk.origin <> v.node)
	           END AS score
	    FROM (SELECT DISTINCT node FROM walk) v
	    JOIN elig e ON e.id = v.node
	    LEFT JOIN matched m ON m.id = v.node
	)`
	} else {
		visible = `,
	tree AS (
	    SELECT id, parent_id FROM matched
	    UNION
	    SELECT e.id, e.parent_id FROM elig e JOIN tree ON e.id = tree.parent_id
	),
	vrow AS (
	    SELECT e.* FROM elig e JOIN tree ON e.id = tree.id
	)`
	}

	pageWhere := ""
	if parent == "" {
		pageWhere = "parent_id IS NULL"
	} else {
		pageWhere = "parent_id = " + a.next(parent) + "::uuid"
	}
	switch sort {
	case ListSortCreated:
		if cursorAt != nil {
			tArg := a.next(*cursorAt)
			idArg := a.next(cursorID)
			pageWhere += "\n  AND (created_at, id) < (" + tArg + ", " + idArg + "::uuid)"
		}
	case ListSortRelevance:
		if cursorScore != nil {
			fArg := a.next(*cursorScore)
			idArg := a.next(cursorID)
			pageWhere += "\n  AND (score, id) > (" + fArg + ", " + idArg + "::uuid)"
		}
	default:
		if cursorAt != nil {
			tArg := a.next(*cursorAt)
			idArg := a.next(cursorID)
			pageWhere += "\n  AND (updated_at, id) < (" + tArg + ", " + idArg + "::uuid)"
		}
	}
	limArg := a.next(limit + 1)

	order := ""
	// outerOrder qualifies every term: a bare id would resolve to the
	// p.id::text output column and order ties by text, while the page and the
	// keyset order by uuid.
	outerOrder := ""
	switch sort {
	case ListSortCreated:
		order = "created_at DESC, id DESC"
		outerOrder = "p.created_at DESC, p.id DESC"
	case ListSortRelevance:
		order = "score ASC, id ASC"
		outerOrder = "p.score ASC, p.id ASC"
	default:
		order = "updated_at DESC, id DESC"
		outerOrder = "p.updated_at DESC, p.id DESC"
	}

	query := `WITH RECURSIVE elig AS (` + elig + `
),
matched AS (` + matched + `)` + visible + `,
page AS (
    SELECT * FROM vrow
    WHERE ` + pageWhere + `
    ORDER BY ` + order + `
    LIMIT ` + limArg + `
)
SELECT p.id::text,
       p.name,
       p.model,
       p.owner,
       p.created_at,
       p.updated_at,
       p.closed_at,
       coalesce(tt.turns, 0),
       coalesce(tc.cost_usd, 0),
       coalesce(p.parent_id::text, ''),
       cc.n,
       coalesce(p.child_id, ''),
       coalesce(p.child_name, ''),
       coalesce(p.child_kind, ''),
       p.state` + scoreColumn(sort) + `
FROM page p
LEFT JOIN LATERAL (
    SELECT count(*) AS turns
    FROM conversations.conversation_turn WHERE conversation_id = p.id
) tt ON true
LEFT JOIN LATERAL (
    SELECT coalesce(sum(
        coalesce(t2.input_tokens, 0)       * coalesce(mp.prompt_usd, 0) +
        coalesce(t2.output_tokens, 0)      * coalesce(mp.completion_usd, 0) +
        coalesce(t2.cache_read_tokens, 0)  * coalesce(mp.cache_read_usd, 0)
    ), 0) AS cost_usd
    FROM conversations.conversation_turn t2
    LEFT JOIN conversations.model_pricing mp ON mp.model_id = t2.model
    WHERE t2.conversation_id = p.id
) tc ON true
LEFT JOIN LATERAL (
    SELECT count(*)::int AS n FROM vrow vv WHERE vv.parent_id = p.id
) cc ON true
ORDER BY ` + outerOrder

	rows, err := i.pool.Query(ctx, query, a.args...)
	if err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	out := make([]ConversationListing, 0, limit)
	// scores rides alongside out for the relevance sort only: the cursor
	// needs the last returned row's score, which the listing does not carry.
	scores := make([]float64, 0, limit)
	for rows.Next() {
		var l ConversationListing
		var state string
		dest := []any{
			&l.ID, &l.Name, &l.Model, &l.Owner,
			&l.CreatedAt, &l.LastActivity, &l.ClosedAt,
			&l.Turns, &l.CostUSD,
			&l.ParentConversationID, &l.ChildCount,
			&l.ChildID, &l.ChildName, &l.Kind,
			&state,
		}
		var score float64
		if sort == ListSortRelevance {
			dest = append(dest, &score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, "", fmt.Errorf("scan conversation listing: %w", err)
		}
		l.State = ListState(state)
		out = append(out, l)
		scores = append(scores, score)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		scores = scores[:limit]
		last := out[len(out)-1]
		var at *time.Time
		var sc *float64
		switch sort {
		case ListSortCreated:
			// The keyset compares the sort column: created_at here,
			// updated_at for the recent sort.
			at = &last.CreatedAt
		case ListSortRelevance:
			sc = &scores[len(out)-1]
		default:
			at = &last.LastActivity
		}
		next = encodeListCursor(sort, parent, last.ID, at, sc)
	}
	return out, next, nil
}

// scoreColumn appends the trailing score output column for the relevance sort.
func scoreColumn(sort ListSort) string {
	if sort == ListSortRelevance {
		return ",\n       p.score"
	}
	return ""
}

// bm25WindowAvailable reports whether the BM25 index over conversation windows
// exists on this database. Recall search installs it with the pg_textsearch
// extension; an install without it cannot answer a Query.
func (i *Insights) bm25WindowAvailable(ctx context.Context) (bool, error) {
	var ok bool
	err := i.pool.QueryRow(ctx,
		"SELECT to_regclass('"+bm25WindowIndex+"') IS NOT NULL").Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", bm25WindowIndex, err)
	}
	return ok, nil
}

// Visible reports whether conversationID is inside scope. Unknown and
// malformed ids answer identically: (false, nil).
func (i *Insights) Visible(ctx context.Context, scope Scope, conversationID string) (bool, error) {
	if !strictUUID(conversationID) {
		return false, nil
	}
	var a argList
	query := `SELECT EXISTS (
	    SELECT 1 FROM conversations.conversation c
	    WHERE c.id = ` + a.next(conversationID) + `::uuid
	      AND ` + scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref") + `)`
	var ok bool
	if err := i.pool.QueryRow(ctx, query, a.args...).Scan(&ok); err != nil {
		return false, fmt.Errorf("visible conversation: %w", err)
	}
	return ok, nil
}
