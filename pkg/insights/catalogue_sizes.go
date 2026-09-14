// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerQuery("sizes", ClassOwnerScoped, querySizes)
}

// sizeBuckets are fixed, named turn-count ranges. Chosen from the corpus's
// own observed spread (tasks/conversation-queries.md §3.3: worker/other avg
// 57, coordinator avg 479 max 6248) so the buckets actually separate the
// classes rather than lumping everything into one.
var sizeBucketOrder = []string{"<25", "25-100", "100-250", "250-500", "500+"}

func sizeBucket(turns int64) string {
	switch {
	case turns < 25:
		return "<25"
	case turns < 100:
		return "25-100"
	case turns < 250:
		return "100-250"
	case turns < 500:
		return "250-500"
	default:
		return "500+"
	}
}

// querySizes gives a turn-count HISTOGRAM per behavioral class -- how many
// conversations in each class fall into each size bucket -- using the same
// classification as queryClasses (see that function's doc comment). This is
// new information queryClasses' own avg/max summary doesn't carry (e.g.
// whether a class is bimodal, or its max is a lone outlier).
func querySizes(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error) {
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

	// Bucketing happens in Go, not SQL: the query returns one row per
	// (conversation, turns) and sizeBucket() classifies it, so
	// sizeBucketOrder stays the single source of truth for both the bucket
	// boundaries and their display order.
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
  t.turns
FROM cls
JOIN conversations.conversation c ON c.id = cls.cid
JOIN LATERAL (SELECT count(*) AS turns FROM conversations.conversation_turn ct
              WHERE ct.conversation_id = cls.cid) t ON true
WHERE ` + strings.Join(conds, " AND ")

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query sizes: %w", err)
	}
	defer rows.Close()

	type key struct{ class, bucket string }
	counts := map[key]int64{}
	for rows.Next() {
		var class string
		var turns int64
		if err := rows.Scan(&class, &turns); err != nil {
			return QueryResult{}, fmt.Errorf("query sizes: scan: %w", err)
		}
		counts[key{class, sizeBucket(turns)}]++
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, fmt.Errorf("query sizes: %w", err)
	}

	result := QueryResult{Columns: []Column{
		{Name: "class", Kind: ColString},
		{Name: "size_bucket", Kind: ColString},
		{Name: "conversations", Kind: ColInt},
	}}
	// Deterministic order: class alphabetical, bucket by sizeBucketOrder --
	// a map iterated directly would make row order flaky across runs.
	classesSeen := map[string]bool{}
	for k := range counts {
		classesSeen[k.class] = true
	}
	sortedClasses := make([]string, 0, len(classesSeen))
	for c := range classesSeen {
		sortedClasses = append(sortedClasses, c)
	}
	sort.Strings(sortedClasses)
	for _, class := range sortedClasses {
		for _, bucket := range sizeBucketOrder {
			n, ok := counts[key{class, bucket}]
			if !ok {
				continue
			}
			result.Rows = append(result.Rows, []Entry{StringEntry(class), StringEntry(bucket), IntEntry(n)})
		}
	}
	return result, nil
}
