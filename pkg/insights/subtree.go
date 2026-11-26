// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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
//   - ExternalRefPrefixes — the proxy path's THREAD BRANCHES, whose ref is
//     "<childID>:<turn-uuid>". Most branches are a real Task subagent with a
//     synthetic child of its own, already named by ExternalRefs; this list
//     exists for the ones that are not. Claude Code's WebFetch and WebSearch
//     helpers fork a branch per call and get no child record (they declare no
//     client tools, so they are not agents), and without a prefix nothing
//     reaches their spend — ~2.3M haiku tokens on one measured session. They
//     are a tool call the parent made, so they are the parent's spend.
//
// A rollup that follows only one of these under-reports a mixed subtree, and
// under-reporting a budget is the failure direction that costs money.
type SubtreeSelector struct {
	ConversationIDs     []string
	ExternalRefs        []string
	ExternalRefPrefixes []string
}

func (s SubtreeSelector) empty() bool {
	return len(s.ConversationIDs) == 0 && len(s.ExternalRefs) == 0 &&
		len(s.ExternalRefPrefixes) == 0
}

// refMatch is the WHERE clause shared by both rollup queries. starts_with
// rather than LIKE because a child id contains '_' ("c_01M2…"), which LIKE
// reads as a single-character wildcard — an escaping bug that would silently
// over-match a sibling.
const refMatch = `(c.id = ANY($1::uuid[])
	 OR c.external_ref = ANY($2::text[])
	 OR EXISTS (SELECT 1 FROM unnest($3::text[]) p WHERE starts_with(c.external_ref, p)))`

// ConversationUsage is one conversation's token usage on one model.
//
// It is deliberately UNPRICED: the same row can be priced by the daemon's
// catalog pricer and by nothing else, and a caller that memoises spend across
// turns (Task 3.1's union of memoised and fresh rows) must be able to add
// tokens up before any price is applied. Tokens are the durable fact; dollars
// are a derived view of them (PriceUsage).
type ConversationUsage struct {
	ConversationID                 string
	ExternalRef                    string
	Model                          string
	In, Out, CacheRead, CacheWrite int64
}

// UsageByConversation returns the selected conversations' token sums per
// (conversation, model), unpriced, in one round trip.
//
// This is the raw form of CostsByConversation: one row per conversation per
// model, so a caller can price each conversation separately, sum a subtree, or
// total a model across conversations. Grouping by conversation AND model (not
// by model like SubtreeCost's old query) is what keeps the per-conversation
// rollup available without a second round trip.
func (i *Insights) UsageByConversation(ctx context.Context, sel SubtreeSelector) ([]ConversationUsage, error) {
	if sel.empty() {
		return nil, nil
	}

	// uuidsOnly, same as CostsByConversation: $1::uuid[] rejects the whole
	// ARRAY on one malformed element, so a single non-UUID session id in the
	// batched selector must degrade to "row unreachable", never fail the
	// rollup.
	rows, err := i.pool.Query(ctx,
		`SELECT c.id::text, coalesce(c.external_ref,''), coalesce(t.model,''), `+tokenSums+` `+statsFrom+`
		 WHERE `+refMatch+`
		 GROUP BY c.id, c.external_ref, t.model`,
		uuidsOnly(sel.ConversationIDs), nonNilStrings(sel.ExternalRefs),
		nonNilStrings(sel.ExternalRefPrefixes))
	if err != nil {
		return nil, fmt.Errorf("usage by conversation: %w", err)
	}

	var collected []ConversationUsage
	for rows.Next() {
		var r ConversationUsage
		if err := rows.Scan(&r.ConversationID, &r.ExternalRef, &r.Model,
			&r.In, &r.Out, &r.CacheRead, &r.CacheWrite); err != nil {
			rows.Close()
			return nil, fmt.Errorf("usage by conversation: scan: %w", err)
		}
		collected = append(collected, r)
	}
	// Release the cursor BEFORE returning, so the caller can price: the pricer
	// consults the model catalog, and holding an open cursor on the pool across
	// that is how the existing cost() path deadlocked under load. Every caller
	// prices, so the release belongs here rather than in each of them.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage by conversation: rows: %w", err)
	}
	return collected, nil
}

// PriceUsage prices usage rows and returns the models that could not be
// priced.
//
// It is the ONE place usage rows become dollars: SubtreeCostDetailed and
// CostsByConversation both price through it, and anything that prices a union
// of memoised and fresh rows must too, so "what does this subtree cost" cannot
// acquire a second definition. A row whose model has no price contributes
// nothing (the only honest option) and its model is reported once — the
// unpriced list names MODELS, not rows, so a model shared by ten conversations
// is not ten entries. A nil pricer prices nothing and reports every model.
func PriceUsage(rows []ConversationUsage, price Pricer) (total float64, unpriced []string) {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if price != nil {
			if p, ok := price(r.Model); ok {
				total += p.CostOf(r.In, r.Out, r.CacheRead, r.CacheWrite).Total
				continue
			}
		}
		if _, dup := seen[r.Model]; dup {
			continue
		}
		seen[r.Model] = struct{}{}
		unpriced = append(unpriced, r.Model)
	}
	return total, unpriced
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

	usage, err := i.UsageByConversation(ctx, sel)
	if err != nil {
		return 0, nil, fmt.Errorf("subtree cost: %w", err)
	}

	// Roll the per-(conversation, model) rows up per MODEL before pricing: a
	// subtree cares about its total, not which conversation paid. Summing here
	// rather than pricing row by row also keeps the unpriced list shaped as it
	// was when the query itself grouped by model. No conversation is
	// double-counted: UsageByConversation groups each conversation once, so a
	// conversation matched by two selector arms is one row.
	perModel := make([]ConversationUsage, 0, len(usage))
	index := make(map[string]int, len(usage))
	for _, u := range usage {
		if n, ok := index[u.Model]; ok {
			perModel[n].In += u.In
			perModel[n].Out += u.Out
			perModel[n].CacheRead += u.CacheRead
			perModel[n].CacheWrite += u.CacheWrite
			continue
		}
		index[u.Model] = len(perModel)
		perModel = append(perModel, ConversationUsage{
			Model: u.Model, In: u.In, Out: u.Out,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		})
	}

	total, unpriced := PriceUsage(perModel, i.pricer)
	return total, unpriced, nil
}

// PriceUsage prices rows with the pricer this Insights was built with, so a
// caller holding a union of memoised and freshly read usage rows (the daemon's
// subtree spend) prices them through the SAME list prices every SQL rollup
// uses. It is the package-level PriceUsage with i.pricer applied — not a second
// pricing rule.
func (i *Insights) PriceUsage(rows []ConversationUsage) (total float64, unpriced []string) {
	return PriceUsage(rows, i.pricer)
}

// nonNilStrings keeps `= ANY($1)` well-typed for an empty list.
// A nil slice binds as SQL NULL, and `x = ANY(NULL)` is NULL rather than
// false — which makes the whole WHERE clause match nothing in a way that looks
// like "the subtree has spent nothing" instead of like a bug. (The UUID arm
// needs the same nil-safety; uuidsOnly provides it — its output is always
// non-nil — and drops malformed elements at the same time.)
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ConversationCost is one conversation's priced spend.
type ConversationCost struct {
	ConversationID string
	ExternalRef    string
	Cost           float64
}

// CostsByConversation prices the selected conversations INDIVIDUALLY, in one
// round trip.
//
// SubtreeCost answers "what has this subtree spent" and collapses everything
// into a single number, which is the right shape for a budget check and the
// wrong one for a list: filling N children's costs with N calls to it is N
// serial round trips, each with its own timeout, on the cockpit's seed path.
//
// Rows are returned rather than a map because a conversation is reachable by
// two different keys and the caller has to decide how to combine them. A map
// keyed by both would silently double a child whose id matches one row by
// UUID and another by external_ref.
func (i *Insights) CostsByConversation(ctx context.Context, sel SubtreeSelector) ([]ConversationCost, error) {
	if sel.empty() {
		return nil, nil
	}

	usage, err := i.UsageByConversation(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("costs by conversation: %w", err)
	}
	// Unpriced means NO rows, not zero-cost rows: the caller must be able to
	// tell "nothing spent" from "we cannot see the price".
	if i.pricer == nil {
		return nil, nil
	}

	// Group the usage rows per conversation and price each group. The group's
	// external_ref is the conversation's own column, so every row agrees on it.
	byConv := make(map[string]*ConversationCost, len(usage))
	grouped := make(map[string][]ConversationUsage, len(usage))
	for _, u := range usage {
		cc := byConv[u.ConversationID]
		if cc == nil {
			cc = &ConversationCost{ConversationID: u.ConversationID, ExternalRef: u.ExternalRef}
			byConv[u.ConversationID] = cc
		}
		grouped[u.ConversationID] = append(grouped[u.ConversationID], u)
	}
	for convID, rows := range grouped {
		// An unpriced model contributes nothing rather than failing the batch:
		// one model with no list price must not blank every other child's cost.
		// The unpriced models are dropped here on purpose — this verb's contract
		// is a cost per conversation; SubtreeCostDetailed reports them.
		cost, _ := PriceUsage(rows, i.pricer)
		byConv[convID].Cost = cost
	}
	out := make([]ConversationCost, 0, len(byConv))
	for _, cc := range byConv {
		out = append(out, *cc)
	}
	return out, nil
}

// uuidsOnly drops anything that is not a well-formed UUID. See
// UsageByConversation for why the whole array is at stake.
func uuidsOnly(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, err := uuid.Parse(v); err == nil {
			out = append(out, v)
		}
	}
	return out
}
