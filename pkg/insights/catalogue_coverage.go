// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("coverage", ClassOwnerScoped, queryCoverage)
}

// queryCoverage reports, per week, how many agent-kind conversations got a
// child row versus how many didn't -- an instrumentation-completeness check,
// not a content question. See tasks/conversation-queries.md §5.2: child rows
// start 2026-09-03, so anything before that reads as 0% by design, not data
// loss. LEFT JOIN c.child is read WITHOUT a closed_at filter deliberately
// (§5.3: Close soft-deletes child rows, and an analysis query must see
// tombstones to get true historical coverage -- going through the child
// store's own List method here would hide 315 of 317 rows).
func queryCoverage(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
	if err := f.Path.validate(); err != nil {
		return QueryResult{}, err
	}
	var a argList
	conds := []string{"c.origin_entrypoint = 'agent'", scope.cond(&a, "c.owner_user_id")}
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
		conds = append(conds, "c.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "c.created_at < "+a.next(*f.Until))
	}

	query := `
SELECT date_trunc('week', c.created_at) AS wk, count(*) AS convs, count(ch.child_id) AS with_child
FROM conversations.conversation c
LEFT JOIN conversations.child ch ON ch.conversation_id = c.id
WHERE ` + strings.Join(conds, " AND ") + `
GROUP BY 1
ORDER BY 1`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query coverage: %w", err)
	}
	defer rows.Close()

	result := QueryResult{Columns: []Column{
		{Name: "week", Kind: ColString},
		{Name: "conversations", Kind: ColInt},
		{Name: "with_child", Kind: ColInt},
		{Name: "coverage", Kind: ColFloat, Format: "pct"},
	}}
	for rows.Next() {
		var wk time.Time
		var convs, withChild int64
		if err := rows.Scan(&wk, &convs, &withChild); err != nil {
			return QueryResult{}, fmt.Errorf("query coverage: scan: %w", err)
		}
		var coverage float64
		if convs > 0 {
			coverage = float64(withChild) / float64(convs)
		}
		result.Rows = append(result.Rows, []Entry{
			StringEntry(wk.Format("2006-01-02")), IntEntry(convs), IntEntry(withChild), FloatEntry(coverage),
		})
	}
	return result, rows.Err()
}
