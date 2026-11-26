// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// perTokenUSD is what countingUsageCoster charges per input token: $1 per 1M, so
// a fixture's dollars are its token count written out in full.
const perTokenUSD = 1e-6

// defaultTokens is what countingUsageCoster answers for a conversation it was
// not given a token count for.
const defaultTokens int64 = 1000

// countingUsageCoster counts usage reads and answers one row per conversation it
// was asked for, so a test can see exactly which members a read covered and how
// often each was read.
//
// Its price VARIES with the rows it is handed — a per-token rate, not a constant
// — because a constant would let a bug that prices the whole union for every
// child pass: the number would be the same whichever rows it was given.
type countingUsageCoster struct {
	calls     int
	selectors []insights.SubtreeSelector
	// tokens is the input-token count a conversation id answers with; an
	// unlisted id answers defaultTokens.
	tokens map[string]int64
	// unpriced makes PriceUsage report its models as unpriceable (total 0), the
	// shape a model missing from the catalog produces.
	unpriced bool
	// err fails the usage read, the fail-closed case.
	err error
}

func (c *countingUsageCoster) UsageByConversation(_ context.Context, sel insights.SubtreeSelector) ([]insights.ConversationUsage, error) {
	c.calls++
	c.selectors = append(c.selectors, sel)
	if c.err != nil {
		return nil, c.err
	}
	var rows []insights.ConversationUsage
	for _, conv := range sel.ConversationIDs {
		in := defaultTokens
		if v, ok := c.tokens[conv]; ok {
			in = v
		}
		rows = append(rows, insights.ConversationUsage{ConversationID: conv, Model: "test/model", In: in})
	}
	return rows, nil
}

func (c *countingUsageCoster) PriceUsage(rows []insights.ConversationUsage) (float64, []string) {
	if c.unpriced {
		// The real pricing function, unpriced: it names each unpriced MODEL once.
		return insights.PriceUsage(rows, nil)
	}
	var total float64
	for _, r := range rows {
		total += float64(r.In) * perTokenUSD
	}
	return total, nil
}

func (c *countingUsageCoster) CostsByConversation(context.Context, insights.SubtreeSelector) ([]insights.ConversationCost, error) {
	return nil, nil
}

// usdOf is the fixture price of an input-token count.
func usdOf(tokens int64) float64 { return float64(tokens) * perTokenUSD }

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

// warnCapture collects WARN-and-above slog records with their attributes, so a
// test can assert how loudly and how often something was reported.
type warnCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (w *warnCapture) Enabled(context.Context, slog.Level) bool { return true }

func (w *warnCapture) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		w.records = append(w.records, r.Clone())
	}
	return nil
}

func (w *warnCapture) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCapture) WithGroup(string) slog.Handler      { return w }

// recordsText flattens the captured records (level, message, attributes) into
// one string.
func (w *warnCapture) recordsText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	for _, r := range w.records {
		fmt.Fprintf(&b, "%s %s", r.Level, r.Message)
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
			return true
		})
		b.WriteByte('\n')
	}
	return b.String()
}

// count returns how many records were captured.
func (w *warnCapture) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.records)
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

// The batched, memoised spend must equal the number the DELETED selector
// produced: insights.SubtreeCost over the live in-memory set UNIONED with the
// database lineage, exactly what subtreeSelector used to build. The union is
// rebuilt here BY HAND, not from the lineage members, so this compares two
// genuinely different definitions of the subtree.
//
// The fixture holds a root and two live children in the store (one of which, the
// store-only child, has written no turns and has NO lineage row), a CLOSED child
// the live store has forgotten and only the lineage still knows, and one
// conversation reachable from TWO members (an open child's thread prefix and the
// closed child's conversation id) — so a double count, a dropped closed member
// or a store-only member wrongly counted all show up as a different total.
//
// The store-only child writes nothing on purpose: a child in the live set with
// NO row and WITH spend is the one case where the two definitions disagree, and
// the daemon does not produce it — a fresh spawn is refused unless its row is
// written, so every child that can spend is in the lineage.
//
// Spend is computed twice: the second pass answers the closed member from the
// memo, which must not change the answer.
func TestSubtreeSpendMatchesSubtreeCost(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	ctx := context.Background()

	uniq := fmt.Sprintf("c_spendmatch%d", time.Now().UnixNano())
	root, openKid, closedKid := uniq+"_root", uniq+"_open", uniq+"_closed"
	storeOnlyKid := uniq + "_storeonly"
	convRoot := insertConvForCost(t, pool, root)
	convOpen := insertConvForCost(t, pool, openKid)
	convClosed := insertConvForCost(t, pool, closedKid)
	convShared := insertConvForCost(t, pool, openKid+threadRefSep+"helper")
	cleanupConvs(t, pool, convRoot, convOpen, convClosed, convShared)
	for _, conv := range []string{convRoot, convOpen, convClosed, convShared} {
		insertTurnForCost(t, pool, conv, "test/model", 1000, 100)
	}

	// The LIVE set: root, an open child, and a store-only child with no row.
	st := childstore.New()
	for _, id := range []string{root, openKid, storeOnlyKid} {
		labels := map[string]string{}
		if id != root {
			labels[childstore.LabelParent] = root
			labels[childstore.LabelRoot] = root
		}
		st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
			StartedAt: time.Now(), Labels: labels,
		})
	}

	// The database lineage: the same root and open child, plus the closed one.
	closed := lineageRow(closedKid, root, root, "", true)
	closed.Member.ConversationID = convShared // the same conversation, by UUID
	members := []childstore.LineageRow{
		lineageRow(root, "", "", "", false),
		lineageRow(openKid, root, root, "", false),
		closed,
	}
	coster := pricedUsageCoster(t, pool)
	c := &Controller{st: st, coster: coster}
	c.lineage = &fakeLineageSource{rows: members}

	// The OLD selector, rebuilt exactly as the deleted subtreeSelector built it:
	// the live set (the root itself plus its descendants) UNIONED with the
	// database lineage, all three correlation routes.
	var oldSel insights.SubtreeSelector
	addOld := func(childID string, convs ...string) {
		for _, conv := range convs {
			if conv != "" {
				oldSel.ConversationIDs = append(oldSel.ConversationIDs, conv)
			}
		}
		oldSel.ExternalRefs = append(oldSel.ExternalRefs, childID)
		oldSel.ExternalRefPrefixes = append(oldSel.ExternalRefPrefixes, childID+threadRefSep)
	}
	if snap, ok := st.Get(root); ok {
		addOld(snap.ChildID, snap.SessionID)
	}
	for _, snap := range st.Descendants(root) {
		addOld(snap.ChildID, snap.SessionID)
	}
	for _, r := range members {
		addOld(r.Member.ChildID, r.Member.ConversationID, r.Member.SessionID)
	}

	// The two definitions really do differ on this fixture, which is the point.
	newSel, err := c.subtreeSelector(ctx, root)
	ck.Require().NoError(err, "subtree selector")
	ck.Contains(oldSel.ExternalRefs, storeOnlyKid, "the live set names the store-only child")
	ck.NotContains(newSel.ExternalRefs, storeOnlyKid, "the database lineage does not — the fixture exercises the difference")

	want, err := coster.in.SubtreeCost(ctx, oldSel)
	ck.Require().NoError(err, "SubtreeCost over the live+lineage union")
	ck.NotZero(want, "the fixture must have spend for the comparison to mean anything")

	for pass := 0; pass < 2; pass++ {
		got, err := c.subtreeSpend(ctx, root)
		ck.Require().NoError(err, "subtree spend, pass %d", pass)
		ck.LessOrEqual(1e-12, math.Abs(got-want),
			"pass %d: subtree spend = %v, SubtreeCost over the live+lineage union = %v", pass, got, want)
	}
	ck.Eq(2, coster.calls, "one usage read per spend (the closed member is not re-read)")
}

// The memo is filled on the first read of a CLOSED member and never again; an
// OPEN member is read every time. Two spend calls therefore issue two usage
// reads, and only the first names the closed member — while its rows still reach
// the total on both.
func TestClosedUsageMemoFilledOnceNeverForOpen(t *testing.T) {
	ck := assert.NewCollecting(t)
	rootConv := "11111111-1111-1111-1111-111111111111"
	openConv := "22222222-2222-2222-2222-222222222222"
	closedConv := "33333333-3333-3333-3333-333333333333"
	// The closed member spent five times the others, so a memo that dropped its
	// rows is visible in the total, not just in the selector.
	closedTokens := int64(5000)
	coster := &countingUsageCoster{tokens: map[string]int64{closedConv: closedTokens}}
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_root", "", "", rootConv, false),
		lineageRow("c_open", "c_root", "c_root", openConv, false),
		lineageRow("c_closed", "c_root", "c_root", closedConv, true),
	}}
	want := usdOf(defaultTokens) + usdOf(defaultTokens) + usdOf(closedTokens)

	for pass := 0; pass < 2; pass++ {
		spent, err := c.subtreeSpend(context.Background(), "c_root")
		ck.Require().NoError(err, "subtree spend, pass %d", pass)
		ck.LessOrEqual(1e-12, math.Abs(spent-want),
			"pass %d: spend = %v, want %v — the closed member's rows must reach the total from the memo too",
			pass, spent, want)
	}

	ck.Eq(2, coster.calls, "one usage read per spend call")
	ck.Eq(1, countOccurrences(coster.selectors[0].ConversationIDs, closedConv), "the closed member is read once (pass 0)")
	ck.Eq(0, countOccurrences(coster.selectors[1].ConversationIDs, closedConv), "the closed member is NOT read again (pass 1)")
	ck.Eq(1, countOccurrences(coster.selectors[0].ConversationIDs, openConv), "an open member is read on pass 0")
	ck.Eq(1, countOccurrences(coster.selectors[1].ConversationIDs, openConv), "and on pass 1: an open member is never memoised")
}

// An empty child id is refused: the key is what ties a row set to its member, so
// a blank one would let one member's rows answer for every empty-id member.
func TestClosedUsageMemoRefusesAnEmptyChildID(t *testing.T) {
	ck := assert.NewCollecting(t)
	var memo closedUsageMemo
	rows := []insights.ConversationUsage{{ConversationID: "conv-1", Model: "test/model"}}

	memo.put("", rows)
	_, ok := memo.get("")
	ck.False(ok, "a blank child id must never be memoised")

	memo.put("c_closed", rows)
	got, ok := memo.get("c_closed")
	ck.True(ok, "a real child id is memoised")
	ck.Eq(1, len(got), "and keeps its own rows")
}

// K script children under ONE root cost one lineage read and one usage read for
// the whole list. N per-child subtree spends would be N of each, on the
// cockpit's seed path.
//
// Each child reports its OWN subset's price, not the union's: the fake's price
// varies with the rows it is handed, so a child priced from every row would
// report the union total and fail here.
func TestSummariesForOneLineageAndOneUsageQueryPerRoot(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: "c_root", Kind: protocol.KindFundi,
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	coster := &countingUsageCoster{tokens: map[string]int64{}}
	rows := []childstore.LineageRow{lineageRow("c_root", "", "", "", false)}
	wantByChild := map[string]float64{}
	var unionTokens int64
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c_s%d", i)
		conv := fmt.Sprintf("conv-%d", i)
		tokens := int64(1000 * (i + 1))
		st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindScript,
			Status: protocol.StatusIdle, StartedAt: time.Now(),
			Labels: map[string]string{childstore.LabelParent: "c_root", childstore.LabelRoot: "c_root"},
		})
		rows = append(rows, lineageRow(id, "c_root", "c_root", conv, false))
		coster.tokens[conv] = tokens
		wantByChild[id] = usdOf(tokens)
		unionTokens += tokens
	}
	lineage := &fakeLineageSource{rows: rows}
	c := &Controller{st: st, coster: coster}
	c.lineage = lineage

	sums := c.ListChildren(nil)

	ck.Require().Len(sums, 4, "the root and its three script children")
	ck.Eq(1, lineage.calls, "one lineage read for the whole list")
	ck.Eq("c_root", lineage.gotAncestor, "the read is scoped by the ROOT, not by each script child")
	ck.Eq(1, coster.calls, "one usage read for every script child")
	unionPrice := usdOf(unionTokens)
	scripts := 0
	for _, s := range sums {
		if s.Kind != protocol.KindScript {
			continue
		}
		scripts++
		ck.Require().NotNil(s.CostUSD, "script child %s has no CostUSD", s.ChildID)
		ck.LessOrEqual(1e-12, math.Abs(*s.CostUSD-wantByChild[s.ChildID]),
			"script child %s costs %v, want its OWN subset's price %v (the union would be %v)",
			s.ChildID, *s.CostUSD, wantByChild[s.ChildID], unionPrice)
		ck.NotEq(unionPrice, *s.CostUSD, "script child %s must not be priced from every row", s.ChildID)
	}
	ck.Eq(3, scripts, "script children priced")
}

// A failing usage read on the budget path FAILS CLOSED: the spend is unknown, so
// the caller gets an error rather than a $0 subtree that would let a child run
// past its cap.
func TestSubtreeSpendPropagatesAUsageError(t *testing.T) {
	ck := assert.NewCollecting(t)
	coster := &countingUsageCoster{err: errors.New("usage query blew up")}
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_root", "", "", "11111111-1111-1111-1111-111111111111", false),
	}}

	spent, err := c.subtreeSpend(context.Background(), "c_root")
	ck.Require().Error(err, "an unreadable usage read must not price as 0")
	ck.StrContains(err.Error(), "usage query blew up", "the cause must reach the caller; got %v", err)
	ck.Eq(0.0, spent, "no number is reported alongside the error")
	ck.Eq(1, coster.calls, "the read was attempted once")
}

// A failing usage read on the DISPLAY path degrades instead: the script child's
// CostUSD stays unset (nil, never a measured-looking 0) and the rest of the list
// answers normally.
func TestScriptSpendsUsageErrorLeavesCostUnset(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: "c_root", Kind: protocol.KindFundi,
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	st.Insert(&childstore.Session{
		ChildID: "c_script", Kind: protocol.KindScript,
		Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "c_root", childstore.LabelRoot: "c_root"},
	})
	coster := &countingUsageCoster{err: errors.New("usage down")}
	c := &Controller{st: st, coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_root", "", "", "", false),
		lineageRow("c_script", "c_root", "c_root", "conv-script", false),
	}}

	sums := c.ListChildren(nil)

	ck.Require().Len(sums, 2, "the list answers in full despite the failed usage read")
	for _, s := range sums {
		if s.Kind != protocol.KindScript {
			continue
		}
		ck.True(s.CostUSD == nil, "script CostUSD = %v, want unset: nothing measured this subtree", s.CostUSD)
	}
	ck.Eq(1, coster.calls, "the script spend attempted its read once")
}

// An unpriced model records as $0, which is what a child's lifetime maxCost is
// enforced against — so it must be reported at WARN, once per model however many
// conversations spent on it, not whispered at Debug.
func TestSubtreeSpendWarnsOncePerUnpricedModel(t *testing.T) {
	ck := assert.NewCollecting(t)

	prev := slog.Default()
	capture := &warnCapture{}
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Three members, three conversations, ONE model, and nothing can price it.
	coster := &countingUsageCoster{unpriced: true}
	c := &Controller{st: childstore.New(), coster: coster}
	c.lineage = &fakeLineageSource{rows: []childstore.LineageRow{
		lineageRow("c_root", "", "", "11111111-1111-1111-1111-111111111111", false),
		lineageRow("c_a", "c_root", "c_root", "22222222-2222-2222-2222-222222222222", false),
		lineageRow("c_b", "c_root", "c_root", "33333333-3333-3333-3333-333333333333", false),
	}}

	spent, err := c.subtreeSpend(context.Background(), "c_root")
	ck.Require().NoError(err, "an unpriced model is not an error: the turns ran")
	ck.Eq(0.0, spent, "unpriced usage contributes $0, which is the whole problem")

	ck.Eq(1, capture.count(), "one WARN for the spend, not one per conversation")
	text := capture.recordsText()
	ck.StrContains(text, "WARN", "unpriced spend must be reported above Debug; got %q", text)
	ck.StrContains(text, "$0", "the warning must say the usage counts as zero; got %q", text)
	ck.StrContains(text, "c_root", "the warning must name the subtree; got %q", text)
	ck.Eq(1, strings.Count(text, "test/model"),
		"the unpriced model must be named once, not once per conversation; got %q", text)
}
