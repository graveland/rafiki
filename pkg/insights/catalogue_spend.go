// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	registerPricedQuery("spend", ClassOwnerScoped, querySpend)
}

// querySpend reports turns and token sums per UTC day, model and served
// provider, pricing each row with the injected Pricer. The priced column is 0
// when the row's model could not be priced (no Pricer, or no resolvable list
// price); that makes cost_usd UNKNOWN, so a consumer must render it as "-",
// never as $0.
func querySpend(ctx context.Context, pool *pgxpool.Pool, pricer Pricer, scope Scope, f StatsFilter) (QueryResult, error) {
	if err := f.Path.validate(); err != nil {
		return QueryResult{}, err
	}
	var a argList
	conds := []string{scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref")}
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
	if f.Model != "" {
		conds = append(conds, "t.model = "+a.next(f.Model))
	}
	if f.Since != nil {
		conds = append(conds, "t.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "t.created_at < "+a.next(*f.Until))
	}

	// An unknown model is a row, not an exclusion: model is COALESCEd to ''
	// rather than filtered on IS NOT NULL, so unpriced/unknown-model turns
	// still show up (priced = 0) instead of silently vanishing from spend.
	query := `
SELECT to_char(date_trunc('day', t.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
       coalesce(t.model, '') AS model,
       coalesce(t.served_provider, '') AS provider,
       count(*) AS turns,
       coalesce(sum(t.input_tokens),0), coalesce(sum(t.output_tokens),0),
       coalesce(sum(t.cache_read_tokens),0), coalesce(sum(t.cache_creation_tokens),0)
FROM conversations.conversation_turn t
JOIN conversations.conversation c ON c.id = t.conversation_id
WHERE ` + strings.Join(conds, " AND ") + `
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3`

	rows, err := pool.Query(ctx, query, a.args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query spend: %w", err)
	}

	type spendRow struct {
		day, model, provider               string
		turns, in, out, cacheRead, cacheCr int64
	}
	var scanned []spendRow
	for rows.Next() {
		var r spendRow
		if err := rows.Scan(&r.day, &r.model, &r.provider, &r.turns,
			&r.in, &r.out, &r.cacheRead, &r.cacheCr); err != nil {
			rows.Close()
			return QueryResult{}, fmt.Errorf("query spend: scan: %w", err)
		}
		scanned = append(scanned, r)
	}
	rows.Close() // release the cursor before pricing
	if err := rows.Err(); err != nil {
		return QueryResult{}, fmt.Errorf("query spend: %w", err)
	}

	result := QueryResult{Columns: []Column{
		{Name: "day", Kind: ColString},
		{Name: "model", Kind: ColString},
		{Name: "provider", Kind: ColString},
		{Name: "turns", Kind: ColInt},
		{Name: "input_tokens", Kind: ColInt},
		{Name: "output_tokens", Kind: ColInt},
		{Name: "cache_read_tokens", Kind: ColInt},
		{Name: "cache_creation_tokens", Kind: ColInt},
		{Name: "cost_usd", Kind: ColFloat, Format: "usd"},
		{Name: "priced", Kind: ColInt},
	}}
	// Price only after the DB cursor is released, so a pricer that consults the
	// model catalog can't contend with an open rows cursor on the pool.
	for _, r := range scanned {
		var cost float64
		var priced int64
		if pricer != nil {
			if p, ok := pricer(r.model); ok {
				cost = p.CostOf(r.in, r.out, r.cacheRead, r.cacheCr).Total
				priced = 1
			}
		}
		result.Rows = append(result.Rows, []Entry{
			StringEntry(r.day), StringEntry(r.model), StringEntry(r.provider),
			IntEntry(r.turns), IntEntry(r.in), IntEntry(r.out),
			IntEntry(r.cacheRead), IntEntry(r.cacheCr),
			FloatEntry(cost), IntEntry(priced),
		})
	}
	return result, nil
}
