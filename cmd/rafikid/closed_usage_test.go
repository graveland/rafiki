// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// countingUsageCoster counts usage reads and answers one row per conversation it
// was asked for, so a test can see exactly which members a read covered and how
// often each was read.
type countingUsageCoster struct {
	calls     int
	selectors []insights.SubtreeSelector
	spend     float64
}

func (c *countingUsageCoster) UsageByConversation(_ context.Context, sel insights.SubtreeSelector) ([]insights.ConversationUsage, error) {
	c.calls++
	c.selectors = append(c.selectors, sel)
	var rows []insights.ConversationUsage
	for _, conv := range sel.ConversationIDs {
		rows = append(rows, insights.ConversationUsage{ConversationID: conv, Model: "test/model", In: 1000})
	}
	return rows, nil
}

func (c *countingUsageCoster) PriceUsage([]insights.ConversationUsage) (float64, []string) {
	return c.spend, nil
}

func (c *countingUsageCoster) CostsByConversation(context.Context, insights.SubtreeSelector) ([]insights.ConversationCost, error) {
	return nil, nil
}

// dbUsageCoster is the real insights client behind a call counter, so a spend
// test reads real rows through real SQL and can still see how many reads it
// took. Everything including pricing goes through the one client, which is the
// point: the spend path prices memoised and fresh rows with the coster's own
// pricer.
type dbUsageCoster struct {
	in    *insights.Insights
	calls int
}

func (d *dbUsageCoster) UsageByConversation(ctx context.Context, sel insights.SubtreeSelector) ([]insights.ConversationUsage, error) {
	d.calls++
	return d.in.UsageByConversation(ctx, sel)
}

func (d *dbUsageCoster) PriceUsage(rows []insights.ConversationUsage) (float64, []string) {
	return d.in.PriceUsage(rows)
}

func (d *dbUsageCoster) CostsByConversation(ctx context.Context, sel insights.SubtreeSelector) ([]insights.ConversationCost, error) {
	return d.in.CostsByConversation(ctx, sel)
}

// cleanupConvs hard-deletes the conversations a spend test inserted. The test
// database is shared and never reset, so each fixture names its own rows and
// removes them.
func cleanupConvs(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation_turn WHERE conversation_id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`, ids)
	})
}

// pricedUsageCoster builds the spend source these fixtures use: the real
// insights client over pool, pricing "test/model" at $1/M prompt, $2/M
// completion through the catalog pricer production uses.
func pricedUsageCoster(t *testing.T, pool *pgxpool.Pool) *dbUsageCoster {
	t.Helper()
	cat := seedPricedCatalog(t, "test/model")
	return &dbUsageCoster{in: insights.New(pool).WithPricer(catalogPricer(cat, nil))}
}

// oneTestTurnUSD is what insertTurnForCost(…, "test/model", 1000, 100) costs at
// the fixture's price: 1000 × $1/M in, 100 × $2/M out.
const oneTestTurnUSD = 1000*1e-6 + 100*2e-6

// A claude parent's tree, as the database holds it: the parent's own
// conversation, a native subagent (a child ROW plus the thread conversation it
// claims by "<child id>:<thread>"), and a WebFetch/WebSearch helper thread with
// no child of its own — a tool call the parent made. Spend is the three
// conversations' priced usage, each counted ONCE: the helper thread is the
// parent's, and the subagent's branch belongs to the subagent, not also to the
// parent.
func TestClaudeParentSubtreeSpendCountsEachConversationOnce(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	ctx := context.Background()

	uniq := fmt.Sprintf("c_claudeparent%d", time.Now().UnixNano())
	parent, sub := uniq+"_parent", uniq+"_sub"
	mainConv := insertConvForCost(t, pool, parent)
	subConv := insertConvForCost(t, pool, sub+threadRefSep+"task1")
	helperConv := insertConvForCost(t, pool, parent+threadRefSep+"webfetch1")
	cleanupConvs(t, pool, mainConv, subConv, helperConv)
	for _, conv := range []string{mainConv, subConv, helperConv} {
		insertTurnForCost(t, pool, conv, "test/model", 1000, 100)
	}

	coster := pricedUsageCoster(t, pool)
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow(parent, "", "", "", false),
		lineageRow(sub, parent, parent, "", false),
	}}

	got, err := c.subtreeSpend(ctx, parent)
	ck.Require().NoError(err, "subtree spend")
	ck.Eq(1, coster.calls, "one usage read for the whole subtree")
	ck.LessOrEqual(1e-12, math.Abs(got-3*oneTestTurnUSD),
		"spend = %v, want 3 conversations × %v = %v — each conversation once, the helper thread included and the subagent's branch not double counted",
		got, oneTestTurnUSD, 3*oneTestTurnUSD)
}

// The batched, memoised spend must equal the number the old union selector
// produced: insights.SubtreeCost over every member named by all three routes.
// The fixture carries one open and one closed member, and one conversation
// reachable from TWO members (the open child's thread prefix and the closed
// child's conversation id), so a double count or a dropped closed member shows
// up as a different total.
//
// Spend is computed twice: the second pass answers the closed member from the
// memo, which must not change the answer.
func TestSubtreeSpendMatchesSubtreeCost(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	ctx := context.Background()

	uniq := fmt.Sprintf("c_spendmatch%d", time.Now().UnixNano())
	root, openKid, closedKid := uniq+"_root", uniq+"_open", uniq+"_closed"
	convRoot := insertConvForCost(t, pool, root)
	convOpen := insertConvForCost(t, pool, openKid)
	convClosed := insertConvForCost(t, pool, closedKid)
	convShared := insertConvForCost(t, pool, openKid+threadRefSep+"helper")
	cleanupConvs(t, pool, convRoot, convOpen, convClosed, convShared)
	for _, conv := range []string{convRoot, convOpen, convClosed, convShared} {
		insertTurnForCost(t, pool, conv, "test/model", 1000, 100)
	}

	coster := pricedUsageCoster(t, pool)
	closed := lineageRow(closedKid, root, root, "", true)
	closed.Member.ConversationID = convShared // the same conversation, by UUID
	members := []childstore.LineageRow{
		lineageRow(root, "", "", "", false),
		lineageRow(openKid, root, root, "", false),
		closed,
	}
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: members}

	// The OLD union selector: every member, all three routes, no memo.
	var oldSel insights.SubtreeSelector
	for _, r := range members {
		addLineageMember(&oldSel, r.Member)
	}
	want, err := coster.in.SubtreeCost(ctx, oldSel)
	ck.Require().NoError(err, "SubtreeCost over the union selector")

	for pass := 0; pass < 2; pass++ {
		got, err := c.subtreeSpend(ctx, root)
		ck.Require().NoError(err, "subtree spend, pass %d", pass)
		ck.LessOrEqual(1e-12, math.Abs(got-want),
			"pass %d: subtree spend = %v, SubtreeCost over the union selector = %v", pass, got, want)
	}
	ck.Eq(2, coster.calls, "one usage read per spend (the closed member is not re-read)")
}

// The memo is filled on the first read of a CLOSED member and never again; an
// OPEN member is read every time. Two spend calls therefore issue two usage
// reads, and only the first names the closed member.
func TestClosedUsageMemoFilledOnceNeverForOpen(t *testing.T) {
	ck := assert.NewCollecting(t)
	coster := &countingUsageCoster{spend: 0.5}
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_root", "", "", "11111111-1111-1111-1111-111111111111", false),
		lineageRow("c_open", "c_root", "c_root", "22222222-2222-2222-2222-222222222222", false),
		lineageRow("c_closed", "c_root", "c_root", "33333333-3333-3333-3333-333333333333", true),
	}}

	for pass := 0; pass < 2; pass++ {
		spent, err := c.subtreeSpend(context.Background(), "c_root")
		ck.Require().NoError(err, "subtree spend, pass %d", pass)
		ck.Eq(0.5, spent, "pass %d: the closed member's rows still reach the total", pass)
	}

	ck.Eq(2, coster.calls, "one usage read per spend call")
	closedConv := "33333333-3333-3333-3333-333333333333"
	openConv := "22222222-2222-2222-2222-222222222222"
	ck.Eq(1, countOccurrences(coster.selectors[0].ConversationIDs, closedConv), "the closed member is read once (pass 0)")
	ck.Eq(0, countOccurrences(coster.selectors[1].ConversationIDs, closedConv), "the closed member is NOT read again (pass 1)")
	ck.Eq(1, countOccurrences(coster.selectors[0].ConversationIDs, openConv), "an open member is read on pass 0")
	ck.Eq(1, countOccurrences(coster.selectors[1].ConversationIDs, openConv), "and on pass 1: an open member is never memoised")
}

// K script children under ONE root cost one lineage read and one usage read for
// the whole list. N per-child subtree spends would be N of each, on the
// cockpit's seed path.
func TestSummariesForOneLineageAndOneUsageQueryPerRoot(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: "c_root", Kind: protocol.KindFundi,
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	rows := []childstore.LineageRow{lineageRow("c_root", "", "", "", false)}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c_s%d", i)
		st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindScript,
			Status: protocol.StatusIdle, StartedAt: time.Now(),
			Labels: map[string]string{childstore.LabelParent: "c_root", childstore.LabelRoot: "c_root"},
		})
		rows = append(rows, lineageRow(id, "c_root", "c_root", "", false))
	}
	coster := &countingUsageCoster{spend: 0.25}
	lineage := &fakeLineageSource{rows: rows}
	c := &Controller{st: st, coster: coster}
	c.lineage = lineage

	sums := c.ListChildren(nil)

	ck.Require().Len(sums, 4, "the root and its three script children")
	ck.Eq(1, lineage.calls, "one lineage read for the whole list")
	ck.Eq("c_root", lineage.gotAncestor, "the read is scoped by the ROOT, not by each script child")
	ck.Eq(1, coster.calls, "one usage read for every script child")
	scripts := 0
	for _, s := range sums {
		if s.Kind != protocol.KindScript {
			continue
		}
		scripts++
		ck.Require().NotNil(s.CostUSD, "script child %s has no CostUSD", s.ChildID)
		ck.Eq(0.25, *s.CostUSD, "script child %s", s.ChildID)
	}
	ck.Eq(3, scripts, "script children priced")
}
