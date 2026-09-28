// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
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
