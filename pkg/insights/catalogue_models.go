// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("models", ClassOwnerScoped, queryModels)
}

// queryModels reports served-model distribution by conversation and turn
// count. See tasks/conversation-queries.md §2.4.
func queryModels(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
	if err := f.Path.validate(); err != nil {
		return QueryResult{}, err
	}
	var a argList
	conds := []string{"t.model IS NOT NULL", scope.cond(&a, "c.owner_user_id")}
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
	if f.Source != "" {
		conds = append(conds, "t.source = "+a.next(f.Source))
	}
	if f.Since != nil {
		conds = append(conds, "t.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "t.created_at < "+a.next(*f.Until))
	}

	query := `
SELECT t.model, count(DISTINCT t.conversation_id) AS convs, count(*) AS turns
FROM conversations.conversation_turn t
JOIN conversations.conversation c ON c.id = t.conversation_id
WHERE ` + strings.Join(conds, " AND ") + `
GROUP BY 1
ORDER BY 2 DESC`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()

	result := QueryResult{Columns: []Column{
		{Name: "model", Kind: ColString},
		{Name: "conversations", Kind: ColInt},
		{Name: "turns", Kind: ColInt},
	}}
	for rows.Next() {
		var model string
		var convs, turns int64
		if err := rows.Scan(&model, &convs, &turns); err != nil {
			return QueryResult{}, fmt.Errorf("query models: scan: %w", err)
		}
		result.Rows = append(result.Rows, []Entry{StringEntry(model), IntEntry(convs), IntEntry(turns)})
	}
	return result, rows.Err()
}
