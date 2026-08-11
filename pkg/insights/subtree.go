// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
)

// SubtreeSelector names the conversations belonging to one agent subtree.
//
// Two lists, because a child's conversation is reachable by two different
// routes depending on how it ran, and a subtree routinely mixes them:
//
//   - ConversationIDs — the in-process fundi path, where the daemon knows the
//     conversation UUID directly (childstore.Snapshot.SessionID).
//   - ExternalRefs — the proxy path, where the daemon sets
//     X-Rafiki-Session: <childID> and the row correlates on external_ref.
//
// A rollup that follows only one of these under-reports a mixed subtree, and
// under-reporting a budget is the failure direction that costs money.
type SubtreeSelector struct {
	ConversationIDs []string
	ExternalRefs    []string
}

func (s SubtreeSelector) empty() bool {
	return len(s.ConversationIDs) == 0 && len(s.ExternalRefs) == 0
}

// SubtreeCost returns the total USD spend across the selected conversations.
// An empty selector is 0, not an error: a coordinator with no children has
// spent nothing, and making that a failure would make the first budget check
// of every subtree fail.
func (i *Insights) SubtreeCost(ctx context.Context, sel SubtreeSelector) (float64, error) {
	total, _, err := i.SubtreeCostDetailed(ctx, sel)
	return total, err
}

// SubtreeCostDetailed additionally reports the models that could not be
// priced.
//
// The distinction matters at a budget boundary: "this subtree has spent $2 of
// $10" and "this subtree has spent $2 that we could price plus an unknown
// amount on a model with no list price" are different facts, and silently
// conflating them turns a missing price into an unbounded budget.
func (i *Insights) SubtreeCostDetailed(ctx context.Context, sel SubtreeSelector) (float64, []string, error) {
	if sel.empty() {
		return 0, nil, nil
	}

	rows, err := i.pool.Query(ctx,
		`SELECT coalesce(t.model,''), `+tokenSums+` `+statsFrom+`
		 WHERE c.id = ANY($1::uuid[]) OR c.external_ref = ANY($2::text[])
		 GROUP BY t.model`,
		nonNilUUIDs(sel.ConversationIDs), nonNilStrings(sel.ExternalRefs))
	if err != nil {
		return 0, nil, fmt.Errorf("subtree cost: %w", err)
	}

	type row struct {
		model                           string
		in, out, cacheRead, cacheCreate int64
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.model, &r.in, &r.out, &r.cacheRead, &r.cacheCreate); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("subtree cost: scan: %w", err)
		}
		collected = append(collected, r)
	}
	// Release the cursor BEFORE pricing: the pricer consults the model
	// catalog, and holding an open cursor on the pool across that is how the
	// existing cost() path deadlocked under load.
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("subtree cost: rows: %w", err)
	}

	if i.pricer == nil {
		unpriced := make([]string, 0, len(collected))
		for _, r := range collected {
			unpriced = append(unpriced, r.model)
		}
		return 0, unpriced, nil
	}

	var total float64
	var unpriced []string
	for _, r := range collected {
		p, ok := i.pricer(r.model)
		if !ok {
			unpriced = append(unpriced, r.model)
			continue
		}
		total += p.CostOf(r.in, r.out, r.cacheRead, r.cacheCreate).Total
	}
	return total, unpriced, nil
}

// nonNilUUIDs / nonNilStrings keep `= ANY($1)` well-typed for an empty list.
// A nil slice binds as SQL NULL, and `x = ANY(NULL)` is NULL rather than
// false — which makes the whole WHERE clause match nothing in a way that looks
// like "the subtree has spent nothing" instead of like a bug.
func nonNilUUIDs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
