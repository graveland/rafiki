// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("tools", ClassOwnerScoped, queryTools)
}

// queryTools counts tool_use blocks by tool name, merged CASE-INSENSITIVELY:
// "Bash" and "bash" are one row, displayed under the spelling that carried
// more calls (Postgres mode() is deterministic on a tie -- the sort-first
// spelling wins). Rows sort by calls DESCENDING, then tool name; the
// conversations column counts DISTINCT conversations over the whole merged
// group, never a sum of per-spelling counts, or a conversation that used both
// spellings would count twice. The merge deliberately retires the old
// case-preserving contract, which kept Claude Code's tools (capitalised, seen
// through the proxy) apart from fundi's (lowercase) -- see
// tasks/conversation-queries.md §4.2; the dominant spelling still hints at
// which framework dominated a row.
func queryTools(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
	if err := f.Path.validate(); err != nil {
		return QueryResult{}, err
	}
	var a argList
	conds := []string{"1=1", scope.cond(&a, "c.owner_user_id")}
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
WITH usage AS (
    SELECT lower(b->>'name') AS key, b->>'name' AS tool, m.conversation_id AS conv_id
    FROM conversations.conversation_message m
    JOIN conversations.conversation c ON c.id = m.conversation_id
    , LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(m.content) = 'array' THEN m.content ELSE '[]'::jsonb END
      ) b
    WHERE b->>'type' = 'tool_use' AND ` + strings.Join(conds, " AND ") + `
)
SELECT mode() WITHIN GROUP (ORDER BY tool) AS tool,
       count(*) AS calls,
       count(DISTINCT conv_id) AS convs
FROM usage
GROUP BY key
ORDER BY 2 DESC, 1`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query tools: %w", err)
	}
	defer rows.Close()

	result := QueryResult{Columns: []Column{
		{Name: "tool", Kind: ColString},
		{Name: "calls", Kind: ColInt},
		{Name: "conversations", Kind: ColInt},
	}}
	for rows.Next() {
		var tool string
		var n, convs int64
		if err := rows.Scan(&tool, &n, &convs); err != nil {
			return QueryResult{}, fmt.Errorf("query tools: scan: %w", err)
		}
		result.Rows = append(result.Rows, []Entry{StringEntry(tool), IntEntry(n), IntEntry(convs)})
	}
	return result, rows.Err()
}
