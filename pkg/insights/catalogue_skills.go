// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("skills", ClassOwnerScoped, querySkills)
}

// querySkills counts skill invocations by NORMALIZED name: the namespace
// prefix (everything up to and including the last ':') is stripped before
// grouping, so "rafiki:brainstorming", "superpowers:brainstorming", and
// "brainstorming" collapse into one row. Without this, the same skill is
// counted under three spellings -- see tasks/conversation-queries.md §4.3,
// which names this exact fix as needed, not optional.
func querySkills(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
	if err := f.Path.validate(); err != nil {
		return QueryResult{}, err
	}
	var a argList
	conds := []string{"1=1", scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref")}
	if db := f.Path.drivenBy(); db != "" {
		conds = append(conds, "c.driven_by = "+a.next(db))
	}
	if f.Owner != "" {
		conds = append(conds, "c.owner_user_id = (SELECT id FROM conversations.users WHERE username = "+
			a.next(f.Owner)+" ORDER BY created_at DESC LIMIT 1)")
	}
	if f.Persona != "" {
		conds = append(conds, "c.persona = "+a.next(f.Persona))
	}
	if f.Since != nil {
		conds = append(conds, "m.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "m.created_at < "+a.next(*f.Until))
	}

	query := `
SELECT regexp_replace(coalesce(b->'input'->>'skill', b->'input'->>'name', '?'), '^.*:', '') AS skill,
       count(*) AS n, count(DISTINCT m.conversation_id) AS convs
FROM conversations.conversation_message m
JOIN conversations.conversation c ON c.id = m.conversation_id
, LATERAL jsonb_array_elements(
    CASE WHEN jsonb_typeof(m.content) = 'array' THEN m.content ELSE '[]'::jsonb END
  ) b
WHERE b->>'type' = 'tool_use' AND lower(b->>'name') = 'skill' AND ` + strings.Join(conds, " AND ") + `
GROUP BY 1
ORDER BY 2 DESC, 1`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query skills: %w", err)
	}
	defer rows.Close()

	result := QueryResult{Columns: []Column{
		{Name: "skill", Kind: ColString},
		{Name: "invocations", Kind: ColInt},
		{Name: "conversations", Kind: ColInt},
	}}
	for rows.Next() {
		var skill string
		var n, convs int64
		if err := rows.Scan(&skill, &n, &convs); err != nil {
			return QueryResult{}, fmt.Errorf("query skills: scan: %w", err)
		}
		result.Rows = append(result.Rows, []Entry{StringEntry(skill), IntEntry(n), IntEntry(convs)})
	}
	return result, rows.Err()
}
