// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("classes", ClassOwnerScoped, queryClasses)
}

// queryClasses buckets conversations into coordinator/brainstorming/planning/
// worker-other, precedence-ordered in that listed order (a coordinator that
// also brainstormed lands in "coordinator"). See tasks/conversation-queries.md
// §3.3.
func queryClasses(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
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
	var turnConds []string
	if f.Model != "" {
		turnConds = append(turnConds, "tm.model = "+a.next(f.Model))
	}
	if f.Source != "" {
		turnConds = append(turnConds, "tm.source = "+a.next(f.Source))
	}
	if f.Since != nil {
		turnConds = append(turnConds, "tm.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		turnConds = append(turnConds, "tm.created_at < "+a.next(*f.Until))
	}
	if len(turnConds) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM conversations.conversation_turn tm "+
			"WHERE tm.conversation_id = c.id AND "+strings.Join(turnConds, " AND ")+")")
	}

	query := `
WITH tu AS (
  SELECT m.conversation_id AS cid, lower(b->>'name') AS tool,
         lower(coalesce(b->'input'->>'skill', b->'input'->>'name', '')) AS sk
  FROM conversations.conversation_message m,
       LATERAL jsonb_array_elements(
         CASE WHEN jsonb_typeof(m.content) = 'array' THEN m.content ELSE '[]'::jsonb END) b
  WHERE b->>'type' = 'tool_use'
), cls AS (
  SELECT cid,
    bool_or(tool IN ('agent_spawn', 'agent')) AS dispatches,
    bool_or(tool = 'skill' AND sk LIKE '%brainstorm%') AS brainstorm,
    bool_or(tool = 'skill' AND sk LIKE '%plan%') AS plans
  FROM tu GROUP BY cid
)
SELECT
  CASE WHEN cls.dispatches THEN 'coordinator'
       WHEN cls.brainstorm THEN 'brainstorming'
       WHEN cls.plans THEN 'planning'
       ELSE 'worker/other' END AS class,
  count(*) AS convs, round(avg(t.turns)) AS avg_turns, max(t.turns) AS max_turns
FROM cls
JOIN conversations.conversation c ON c.id = cls.cid
JOIN LATERAL (SELECT count(*) AS turns FROM conversations.conversation_turn ct
              WHERE ct.conversation_id = cls.cid) t ON true
WHERE ` + strings.Join(conds, " AND ") + `
GROUP BY 1
ORDER BY 2 DESC`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query classes: %w", err)
	}
	defer rows.Close()

	result := QueryResult{Columns: []Column{
		{Name: "class", Kind: ColString},
		{Name: "conversations", Kind: ColInt},
		{Name: "avg_turns", Kind: ColFloat},
		{Name: "max_turns", Kind: ColInt},
	}}
	for rows.Next() {
		var class string
		var convs, maxTurns int64
		var avgTurns float64
		if err := rows.Scan(&class, &convs, &avgTurns, &maxTurns); err != nil {
			return QueryResult{}, fmt.Errorf("query classes: scan: %w", err)
		}
		result.Rows = append(result.Rows, []Entry{StringEntry(class), IntEntry(convs), FloatEntry(avgTurns), IntEntry(maxTurns)})
	}
	return result, rows.Err()
}
