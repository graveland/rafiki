// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestScriptCostIsSubtreeSpend pins the design rule: a script child's
// cost_usd is its SUBTREE's spend, taken from subtreeSpend — never its own
// conversation rollup, and never the subtree spend leaking onto another
// kind's row. fakeCoster answers 0.42 from SubtreeCost and nothing from
// CostsByConversation, so a 0.42 on the fundi row is exactly the bug this
// test exists to catch.
func TestScriptCostIsSubtreeSpend(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript})
	st.Insert(&childstore.Session{ChildID: "c_fundi", Kind: protocol.KindFundi})
	c := &Controller{coster: fakeCoster{spend: 0.42}, st: st}

	byID := map[string]protocol.ChildSummary{}
	for _, s := range c.ListChildren(nil) {
		byID[s.ChildID] = s
	}

	script, ok := byID["c_script"]
	ck.Require().True(ok, "script child missing from ListChildren")
	ck.Require().NotNil(script.CostUSD, "script CostUSD = nil, want the subtree spend")
	ck.Eq(0.42, *script.CostUSD, "script CostUSD = subtree spend")

	// Other kinds unchanged: the fundi row keeps the rollup's own answer —
	// with this fake the query ran and found no turns, a present zero — and
	// must NOT adopt the script subtree's spend.
	fundi, ok := byID["c_fundi"]
	ck.Require().True(ok, "fundi child missing from ListChildren")
	ck.Require().NotNil(fundi.CostUSD, "fundi CostUSD = nil, want the rollup's present zero")
	ck.Eq(0.0, *fundi.CostUSD, "fundi CostUSD = rollup answer, not the script subtree spend")

	// GetChild answers by the same rule as the list.
	sum, ok := c.GetChild("c_script")
	ck.Require().True(ok, "GetChild(script) ok")
	ck.Require().NotNil(sum.CostUSD, "GetChild: script CostUSD = nil, want the subtree spend")
	ck.Eq(0.42, *sum.CostUSD, "GetChild: script CostUSD = subtree spend")

	// And the fundi row through GetChild too: still the rollup's zero, still
	// not the subtree spend.
	fsum, ok := c.GetChild("c_fundi")
	ck.Require().True(ok, "GetChild(fundi) ok")
	ck.Require().NotNil(fsum.CostUSD, "GetChild: fundi CostUSD = nil, want the rollup's present zero")
	ck.Eq(0.0, *fsum.CostUSD, "GetChild: fundi CostUSD = rollup answer, not the script subtree spend")
}

// TestScriptCostUnsetWithoutCoster pins the error path: with no cost source
// at all — or one that fails — a script child's CostUSD stays UNSET. Nil is
// "not reported"; a reported zero would read as "the subtree has spent
// nothing", which nothing measured.
func TestScriptCostUnsetWithoutCoster(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript})

	// No coster configured: subtreeSpend refuses, the field stays unset.
	c := &Controller{st: st}
	sums := c.ListChildren(nil)
	ck.Require().Len(sums, 1, "ListChildren rows")
	if sums[0].CostUSD != nil {
		ck.Errorf("script CostUSD with no coster = %v, want unset (not reported)", *sums[0].CostUSD)
	}

	// A coster that errors must degrade identically, on both builders.
	c.coster = fakeCoster{err: errors.New("db down")}
	sums = c.ListChildren(nil)
	ck.Require().Len(sums, 1, "ListChildren rows with a failing coster")
	if sums[0].CostUSD != nil {
		ck.Errorf("ListChildren: script CostUSD with a failing coster = %v, want unset", *sums[0].CostUSD)
	}
	sum, ok := c.GetChild("c_script")
	ck.Require().True(ok, "GetChild ok")
	if sum.CostUSD != nil {
		ck.Errorf("GetChild: script CostUSD with a failing coster = %v, want unset", *sum.CostUSD)
	}
}

// ctxRecordingCoster records the context each cost method was handed, so a
// test can pin that a list's rollup and its per-script subtree queries share
// ONE bounded window rather than stacking two sequential timeouts.
type ctxRecordingCoster struct {
	subtreeCtx context.Context
	costsCtx   context.Context
}

func (c *ctxRecordingCoster) SubtreeCost(ctx context.Context, _ insights.SubtreeSelector) (float64, error) {
	c.subtreeCtx = ctx
	return 0.42, nil
}

func (c *ctxRecordingCoster) CostsByConversation(
	ctx context.Context, _ insights.SubtreeSelector,
) ([]insights.ConversationCost, error) {
	c.costsCtx = ctx
	return nil, nil
}

// One bounded window for a whole list's cost work. The rollup and the
// per-script subtree queries run SEQUENTIALLY, so two independent timeout
// windows would give one list a 2×costRollupTimeout latency bound. Both calls
// must therefore observe the same deadline — equal deadlines are the
// observable of one shared context.
func TestSummariesForCostsShareOneBoundedContext(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript})
	st.Insert(&childstore.Session{ChildID: "c_fundi", Kind: protocol.KindFundi})
	coster := &ctxRecordingCoster{}
	c := &Controller{coster: coster, st: st}

	_ = c.ListChildren(nil)

	ck.Require().NotNil(coster.costsCtx, "CostsByConversation never called")
	ck.Require().NotNil(coster.subtreeCtx, "SubtreeCost never called")
	subDL, hasSubDL := coster.subtreeCtx.Deadline()
	costDL, hasCostDL := coster.costsCtx.Deadline()
	ck.True(hasSubDL && hasCostDL, "the cost ctx carries no deadline — the list's cost work is unbounded")
	ck.True(subDL.Equal(costDL),
		"the rollup and the subtree query ran under different deadlines (%v vs %v) — two stacked timeout windows",
		costDL, subDL)
}
