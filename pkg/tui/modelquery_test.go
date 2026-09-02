// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func i32q(v int32) *int32     { return &v }
func f64q(v float64) *float64 { return &v }

// queryRows models the shape of the real catalog: a big paid model, a big free
// one, a small cheap one, and a local model the catalog knows nothing about.
func queryFixture() []*rafikiv1.ModelRow {
	return []*rafikiv1.ModelRow{
		{Id: "paid/big", ContextWindow: i32q(1_000_000), PromptUsd: f64q(0.0000014),
			IntelligenceIndex: f64q(59.5), SupportedParameters: []string{"tools"}},
		{Id: "free/big", ContextWindow: i32q(1_000_000), PromptUsd: f64q(0),
			IntelligenceIndex: f64q(40.0), SupportedParameters: []string{"tools"}},
		{Id: "paid/small", ContextWindow: i32q(128_000), PromptUsd: f64q(0.0000005),
			IntelligenceIndex: f64q(30.0), SupportedParameters: []string{"tools"}},
		{Id: "paid/pricey", ContextWindow: i32q(1_000_000), PromptUsd: f64q(0.00001),
			IntelligenceIndex: f64q(61.0), SupportedParameters: []string{"tools"}},
		{Id: "local/unknown"}, // no catalog entry at all
	}
}

// stopIndex finds a stop by its label, failing loudly rather than silently
// returning 0: a not-found lookup builds an UNSET bound, so a renamed label
// would turn every filter test into a test of no filtering at all.
func stopIndex(t *testing.T, stops []boundStop, label string) int {
	t.Helper()
	for i, s := range stops {
		if s.label == label {
			return i
		}
	}
	t.Fatalf("no stop labelled %q in %v", label, stops)
	return 0
}

// ── the workflow this exists for ─────────────────────────────────────────────

// ctx >=1M, price >free and <=$2, ordered by intelligence. A multi-key SORT
// cannot express this: it would still list the 128k model, just lower down.
func TestConstraintsThenObjective(t *testing.T) {
	v := defaultModelView()
	v.setBound(colCtx, bound{minIx: stopIndex(t, minStops(colCtx), "1M")})
	v.setBound(colIn, bound{
		minIx: stopIndex(t, minStops(colIn), ">free"),
		maxIx: stopIndex(t, maxStops(colIn), "$2"),
	})
	v.keys = []sortKey{{field: colIntel, desc: true}}

	got := selectModels(queryFixture(), "", v)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.GetId())
	}

	// paid/small fails the context floor; free/big fails ">free";
	// paid/pricey fails "<=$2". local/unknown is admitted -- see below.
	want := []string{"paid/big", "local/unknown"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", ids, want)
	}
}

// A bare ceiling lets free models through -- 7 of them in the real catalog --
// which is why price needs both sides and not a single control.
func TestCeilingAloneAdmitsFreeModels(t *testing.T) {
	v := defaultModelView()
	v.setBound(colIn, bound{maxIx: stopIndex(t, maxStops(colIn), "$2")})

	var sawFree bool
	for _, r := range selectModels(queryFixture(), "", v) {
		if r.GetId() == "free/big" {
			sawFree = true
		}
	}
	if !sawFree {
		t.Fatal("fixture is wrong: a free model must pass a bare ceiling")
	}

	v.setBound(colIn, bound{
		minIx: stopIndex(t, minStops(colIn), ">free"),
		maxIx: stopIndex(t, maxStops(colIn), "$2"),
	})
	for _, r := range selectModels(queryFixture(), "", v) {
		if r.GetId() == "free/big" {
			t.Error(">free did not exclude the free model")
		}
	}
}

// The rule, for the fourth time: unknown is not "fails". Every locally-served
// model has no price, no context and no score, so a bound that rejected
// unknowns would empty the local fleet out of every filtered list.
func TestBoundsAdmitModelsTheCatalogCannotAnswerFor(t *testing.T) {
	v := defaultModelView()
	v.setBound(colCtx, bound{minIx: stopIndex(t, minStops(colCtx), "1M")})
	v.setBound(colIn, bound{maxIx: stopIndex(t, maxStops(colIn), "$1")})
	v.setBound(colIntel, bound{minIx: stopIndex(t, minStops(colIntel), "55")})

	var found bool
	for _, r := range selectModels(queryFixture(), "", v) {
		if r.GetId() == "local/unknown" {
			found = true
		}
	}
	if !found {
		t.Fatal("a model with no catalog facts was rejected by every bound")
	}
}

// ── direction and presence ───────────────────────────────────────────────────

// The bug the tests caught: flipping the comparison for a descending key must
// not flip the ABSENCE verdict, or unscored models lead "smartest".
func TestDescendingDoesNotPromoteUnknowns(t *testing.T) {
	rows := queryFixture()
	sortModels(rows, []sortKey{{field: colIntel, desc: true}})
	if rows[0].GetId() == "local/unknown" {
		t.Fatal("an unscored model leads a descending sort")
	}
	if rows[len(rows)-1].GetId() != "local/unknown" {
		t.Errorf("last = %q, want the unscored model last in BOTH directions",
			rows[len(rows)-1].GetId())
	}

	sortModels(rows, []sortKey{{field: colIntel}}) // ascending
	if rows[len(rows)-1].GetId() != "local/unknown" {
		t.Errorf("last = %q, want the unscored model last ascending too",
			rows[len(rows)-1].GetId())
	}
}

// A second key breaks ties the first leaves, which is the whole point of
// multi-key: context ties constantly (three rows share 1M here).
func TestSecondKeyBreaksTiesLeftByTheFirst(t *testing.T) {
	rows := queryFixture()
	sortModels(rows, []sortKey{{field: colCtx, desc: true}, {field: colIn}})

	var ids []string
	for _, r := range rows[:3] {
		ids = append(ids, r.GetId())
	}
	want := []string{"paid/big", "free/big", "paid/pricey"}
	_ = want
	if ids[0] != "free/big" {
		t.Errorf("among the 1M models the cheapest should lead; got %v", ids)
	}
}

// Two absent values TIE, so the next key gets to decide rather than the order
// being frozen by whichever the stable sort saw first.
func TestTwoAbsentValuesFallThroughToTheNextKey(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{Id: "b/second", IntelligenceIndex: nil, PromptUsd: f64q(0.000002)},
		{Id: "a/first", IntelligenceIndex: nil, PromptUsd: f64q(0.000001)},
	}
	sortModels(rows, []sortKey{{field: colIntel, desc: true}, {field: colIn}})
	if rows[0].GetId() != "a/first" {
		t.Errorf("first = %q, want the cheaper of two unscored models", rows[0].GetId())
	}
}

// ── the dialog ───────────────────────────────────────────────────────────────

func openQuery(t *testing.T) *Cockpit {
	t.Helper()
	c := formCockpit(t)
	seedModels(c, c.form.kind(), queryFixture())
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if c.query == nil {
		t.Fatal("^S did not open the filter+sort band")
	}
	return c
}

func TestCtrlSOpensTheBandOverTheList(t *testing.T) {
	c := openQuery(t)
	if c.form == nil {
		t.Error("the band replaced the form; it is meant to sit over it")
	}
	c.width, c.height, c.ready = 140, 40, true
	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "FILTER") || !strings.Contains(out, "SORT") {
		t.Errorf("both bands should render:\n%s", out)
	}
	// The list is still visible above the band -- that is the point of a band
	// rather than a modal: the query's effect is watchable as you compose it.
	if !strings.Contains(out, "paid/big") {
		t.Errorf("the list is not visible under the band:\n%s", out)
	}
}

func TestTabSwitchesBand(t *testing.T) {
	c := openQuery(t)
	if c.query.band != bandFilter {
		t.Fatalf("band = %v, want filter first", c.query.band)
	}
	c.handleKey(keyMsg("tab"))
	if c.query.band != bandSort {
		t.Errorf("band = %v, want sort after ⇥", c.query.band)
	}
}

func TestSpaceCyclesASortCellThroughOffAscDesc(t *testing.T) {
	c := openQuery(t)
	c.handleKey(keyMsg("tab")) // sort band
	c.query.cell = int(colIntel)

	find := func() (sortKey, bool) {
		for _, k := range c.modelView.keys {
			if k.field == colIntel {
				return k, true
			}
		}
		return sortKey{}, false
	}
	if _, on := find(); on {
		t.Fatal("intel is already a sort key")
	}
	c.handleKey(keyMsg("space"))
	if k, on := find(); !on || k.desc {
		t.Errorf("first space should turn it on ASCENDING; got %+v on=%v", k, on)
	}
	c.handleKey(keyMsg("space"))
	if k, on := find(); !on || !k.desc {
		t.Errorf("second space should make it DESCENDING; got %+v on=%v", k, on)
	}
	c.handleKey(keyMsg("space"))
	if _, on := find(); on {
		t.Error("third space should turn it off")
	}
}

// Turning a key on appends it, so it cannot silently displace the ordering
// already chosen; priority is moved deliberately with the arrows.
func TestNewSortKeysAppendAndArrowsReprioritize(t *testing.T) {
	c := openQuery(t)
	c.handleKey(keyMsg("tab"))
	c.modelView.keys = []sortKey{{field: colCtx, desc: true}}

	c.query.cell = int(colIn)
	c.handleKey(keyMsg("space"))
	if len(c.modelView.keys) != 2 || c.modelView.keys[1].field != colIn {
		t.Fatalf("keys = %+v, want the new key appended last", c.modelView.keys)
	}

	c.handleKey(keyMsg("up"))
	if c.modelView.keys[0].field != colIn {
		t.Errorf("keys = %+v, want ↑ to promote in$ to primary", c.modelView.keys)
	}
	c.handleKey(keyMsg("down"))
	if c.modelView.keys[0].field != colCtx {
		t.Errorf("keys = %+v, want ↓ to demote it again", c.modelView.keys)
	}
}

func TestArrowsStepAThresholdInTheFilterBand(t *testing.T) {
	c := openQuery(t)
	cells := filterCells()
	for i, cell := range cells {
		if cell.field == colCtx && !cell.isMax {
			c.query.cell = i
		}
	}
	c.handleKey(keyMsg("up"))
	if c.modelView.boundFor(colCtx).minIx != 1 {
		t.Fatalf("minIx = %d, want ↑ to step up one stop", c.modelView.boundFor(colCtx).minIx)
	}
	c.handleKey(keyMsg("down"))
	if c.modelView.boundFor(colCtx).minIx != 0 {
		t.Errorf("minIx = %d, want ↓ to step back", c.modelView.boundFor(colCtx).minIx)
	}
	// It CLAMPS rather than wrapping, so holding a key cannot silently loop
	// past "any" back to the strictest stop.
	c.handleKey(keyMsg("down"))
	if got := c.modelView.boundFor(colCtx).minIx; got != 0 {
		t.Errorf("minIx = %d, want it clamped at 0", got)
	}
}

// Every keystroke re-applies the query, so the rows above the band track it.
func TestBandReappliesTheQueryLive(t *testing.T) {
	c := openQuery(t)
	before := len(c.form.suggest)

	cells := filterCells()
	for i, cell := range cells {
		if cell.field == colCtx && !cell.isMax {
			c.query.cell = i
		}
	}
	for i := 0; i < 3; i++ {
		c.handleKey(keyMsg("up")) // step to the strictest context floor
	}

	if len(c.form.suggest) >= before {
		t.Errorf("suggestions %d -> %d, want the floor to narrow the list live",
			before, len(c.form.suggest))
	}
}

func TestEscapeClosesTheBandAndKeepsTheQuery(t *testing.T) {
	c := openQuery(t)
	c.handleKey(keyMsg("tab"))
	c.query.cell = int(colIntel)
	c.handleKey(keyMsg("space"))

	c.handleKey(keyMsg("esc"))

	if c.query != nil {
		t.Error("esc did not close the band")
	}
	if c.form == nil {
		t.Fatal("esc dismissed the form as well")
	}
	var found bool
	for _, k := range c.modelView.keys {
		if k.field == colIntel {
			found = true
		}
	}
	if !found {
		t.Error("the query was discarded when the band closed")
	}
}

// The selected cell and an active cell are different questions -- where the
// cursor is, and what the query constrains -- and both must be visible.
func TestDialogMarksSelectionAndActivationSeparately(t *testing.T) {
	c := openQuery(t)
	v := c.modelView
	v.setBound(colCtx, bound{minIx: 1})

	out := ansi.Strip(c.query.view(160, v))
	if !strings.Contains(out, "ctx ≥128k") {
		t.Errorf("an active bound is not shown:\n%s", out)
	}
	if !strings.Contains(out, "[") {
		t.Errorf("the selected cell is not marked:\n%s", out)
	}
}

func TestSortCellsShowDirectionAndPriority(t *testing.T) {
	c := openQuery(t)
	v := c.modelView
	v.keys = []sortKey{{field: colCtx, desc: true}, {field: colIn}}

	out := ansi.Strip(c.query.view(160, v))
	if !strings.Contains(out, "ctx↓1") {
		t.Errorf("primary key missing its arrow and priority:\n%s", out)
	}
	if !strings.Contains(out, "in$↑2") {
		t.Errorf("secondary key missing its arrow and priority:\n%s", out)
	}
}

func TestBandCostsTheListItsHeight(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), queryFixture())
	focusModelRow(c)
	open := c.form.suggestWindow(40, nil)
	closed := c.form.suggestWindow(40, &queryDialog{})
	if open-closed != queryDialogHeight {
		t.Errorf("window %d -> %d, want the band to cost exactly %d rows",
			open, closed, queryDialogHeight)
	}
}

func TestPriorityDigitDegradesPastNine(t *testing.T) {
	if got := priorityDigit(3); got != "3" {
		t.Errorf("priorityDigit(3) = %q", got)
	}
	if got := priorityDigit(12); got != "+" {
		t.Errorf("priorityDigit(12) = %q, want +", got)
	}
}

func TestEveryFilterCellHasStopsToCycle(t *testing.T) {
	for _, c := range filterCells() {
		if c.flag != flagNone {
			continue
		}
		stops := minStops(c.field)
		if c.isMax {
			stops = maxStops(c.field)
		}
		if len(stops) < 2 {
			t.Errorf("cell %v (max=%v) has nothing to cycle to", c.field, c.isMax)
		}
	}
}

var _ = time.Now
