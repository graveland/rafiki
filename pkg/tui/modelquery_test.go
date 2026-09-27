// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).Eq(strings.Join(want, ","), strings.Join(ids, ","), "got %v, want %v", ids, want)
}

// A bare ceiling lets free models through -- 7 of them in the real catalog --
// which is why price needs both sides and not a single control.
func TestCeilingAloneAdmitsFreeModels(t *testing.T) {
	c := assert.NewCollecting(t)
	v := defaultModelView()
	v.setBound(colIn, bound{maxIx: stopIndex(t, maxStops(colIn), "$2")})

	var sawFree bool
	for _, r := range selectModels(queryFixture(), "", v) {
		if r.GetId() == "free/big" {
			sawFree = true
		}
	}
	c.Require().True(sawFree, "fixture is wrong: a free model must pass a bare ceiling")

	v.setBound(colIn, bound{
		minIx: stopIndex(t, minStops(colIn), ">free"),
		maxIx: stopIndex(t, maxStops(colIn), "$2"),
	})
	for _, r := range selectModels(queryFixture(), "", v) {
		c.NotEq("free/big", r.GetId(), ">free did not exclude the free model")
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
	assert.NewAborting(t).True(found, "a model with no catalog facts was rejected by every bound")
}

// ── direction and presence ───────────────────────────────────────────────────

// The bug the tests caught: flipping the comparison for a descending key must
// not flip the ABSENCE verdict, or unscored models lead "smartest".
func TestDescendingDoesNotPromoteUnknowns(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := queryFixture()
	sortModels(rows, []sortKey{{field: colIntel, desc: true}})
	c.Require().NotEq("local/unknown", rows[0].GetId(), "an unscored model leads a descending sort")
	c.Eq("local/unknown", rows[len(rows)-1].GetId(), "last")

	sortModels(rows, []sortKey{{field: colIntel}}) // ascending
	c.Eq("local/unknown", rows[len(rows)-1].GetId(), "last")
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
	assert.NewCollecting(t).Eq("free/big", ids[0], "among the 1M models the cheapest should lead; got %v", ids)
}

// Two absent values TIE, so the next key gets to decide rather than the order
// being frozen by whichever the stable sort saw first.
func TestTwoAbsentValuesFallThroughToTheNextKey(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{Id: "b/second", IntelligenceIndex: nil, PromptUsd: f64q(0.000002)},
		{Id: "a/first", IntelligenceIndex: nil, PromptUsd: f64q(0.000001)},
	}
	sortModels(rows, []sortKey{{field: colIntel, desc: true}, {field: colIn}})
	assert.NewCollecting(t).Eq("a/first", rows[0].GetId(), "first")
}

// ── the dialog ───────────────────────────────────────────────────────────────

// seekCell drives the cursor to one field's column, the way a user would.
func seekCell(t *testing.T, c *Cockpit, want queryRow, col int) {
	t.Helper()
	rows := queryRowsList()
	for i, r := range rows {
		if r == want {
			c.query.row, c.query.col = i, col
			return
		}
	}
	t.Fatalf("no query row for %+v", want)
}

func openQuery(t *testing.T) *Cockpit {
	t.Helper()
	c := formCockpit(t)
	seedModels(c, c.form.kind(), queryFixture())
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.NewAborting(t).NotNil(c.query, "^S did not open the filter+sort band")
	return c
}

func TestCtrlSOpensThePanelOverTheList(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	ck.NotNil(c.form, "the panel replaced the form; it is meant to sit over it")
	c.width, c.height, c.ready = 120, 44, true
	out := ansi.Strip(c.View().Content)
	for _, want := range []string{"FIELD", "MIN", "MAX", "SORT", "tools", "ctx"} {
		ck.StrContains(out, want, "%q missing from the panel:\n", want)
	}
	// The list stays visible above it -- that is the point of a panel rather
	// than a modal: the query's effect is watchable as it is composed.
	ck.StrContains(out, "paid/big", "the list is not visible above the panel:\n")
}

// The table is one row per field, so it never needs to wrap: the horizontal
// version needed sixteen cells side by side and overflowed any terminal under
// about 140 columns.
func TestPanelFitsANarrowTerminal(t *testing.T) {
	c := openQuery(t)
	for _, w := range []int{60, 80, 100} {
		out := ansi.Strip(c.query.view(w, 40, c.modelView))
		for _, line := range strings.Split(out, "\n") {
			// DISPLAY columns, not bytes: the rule is box-drawing runes at
			// three bytes each, and every width helper in this package
			// measures with ansi.StringWidth for exactly this reason.
			got := ansi.StringWidth(line)
			assert.NewCollecting(t).LessOrEqual(w, got, "width %d: line is %d columns: %q", w, got, line)
		}
	}
}

// A short terminal windows the table rather than burying the list.
func TestPanelLeavesRoomForTheList(t *testing.T) {
	c := assert.NewCollecting(t)
	tall := queryWindow(44)
	short := queryWindow(12)
	c.Less(tall, short, "window: tall")
	c.GreaterOrEqual(1, short, "window")
}

// ←/→ walks the columns and SKIPS cells that hold nothing, so space never
// lands somewhere it does nothing.
func TestArrowsSkipUnavailableCells(t *testing.T) {
	c := openQuery(t)
	seekCell(t, c, queryRow{field: colAgentic}, colMinCell)

	c.handleKey(keyMsg("right"))

	// agentic has a min and a sort but no max stop, so → must jump the max.
	assert.NewCollecting(t).Eq(colSortCell, c.query.col, "col")
}

// A capability toggle occupies one column only.
func TestToggleRowsHaveOneCell(t *testing.T) {
	c := assert.NewCollecting(t)
	r := queryRow{flag: flagTools}
	c.True(r.available(colMinCell), "a toggle should live in the first column")
	c.False(r.available(colMaxCell) || r.available(colSortCell), "a toggle has no max and cannot be sorted by")
}

func TestSpaceCyclesASortCellThroughOffAscDesc(t *testing.T) {
	c := openQuery(t)
	seekCell(t, c, queryRow{field: colIntel}, colSortCell)

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
	_, on := find()
	assert.NewCollecting(t).False(on, "third space should turn it off")
}

// Turning a key on appends it, so it cannot silently displace the ordering
// already chosen; priority moves deliberately with ⇧↑/⇧↓.
func TestNewSortKeysAppendAndShiftArrowsReprioritize(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	c.modelView.keys = []sortKey{{field: colCtx, desc: true}}
	seekCell(t, c, queryRow{field: colIn}, colSortCell)

	c.handleKey(keyMsg("space"))
	if len(c.modelView.keys) != 2 || c.modelView.keys[1].field != colIn {
		t.Fatalf("keys = %+v, want the new key appended last", c.modelView.keys)
	}

	c.handleKey(keyMsg("shift+up"))
	ck.Eq(colIn, c.modelView.keys[0].field, "keys = %+v, want ⇧↑ to promote in$ to primary", c.modelView.keys)
	c.handleKey(keyMsg("shift+down"))
	ck.Eq(colCtx, c.modelView.keys[0].field, "keys = %+v, want ⇧↓ to demote it again", c.modelView.keys)
	// Plain ↑/↓ move the CURSOR, never the priority.
	before := append([]sortKey(nil), c.modelView.keys...)
	c.handleKey(keyMsg("up"))
	ck.Eq(before[0].field, c.modelView.keys[0].field, "a plain ↑ reordered the keys; it should only move the cursor")
}

func TestSpaceCyclesAThresholdAndWraps(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	seekCell(t, c, queryRow{field: colCtx}, colMinCell)
	n := len(minStops(colCtx))

	for i := 1; i < n; i++ {
		c.handleKey(keyMsg("space"))
		ck.Require().Eq(i, c.modelView.boundFor(colCtx).minIx, "minIx")
	}
	c.handleKey(keyMsg("space"))
	ck.Eq(0, c.modelView.boundFor(colCtx).minIx, "minIx")
}

// Min and max are separate cells on the same row, which is what lets ">free"
// and "<=$2" both apply to price at once.
func TestMinAndMaxAreIndependentCells(t *testing.T) {
	c := openQuery(t)
	seekCell(t, c, queryRow{field: colIn}, colMinCell)
	c.handleKey(keyMsg("space"))
	seekCell(t, c, queryRow{field: colIn}, colMaxCell)
	c.handleKey(keyMsg("space"))

	b := c.modelView.boundFor(colIn)
	assert.NewCollecting(t).False(b.minIx == 0 || b.maxIx == 0, "bound = %+v, want both sides set", b)
}

// Every keystroke re-applies the query, so the rows above track it live.
func TestPanelReappliesTheQueryLive(t *testing.T) {
	c := openQuery(t)
	before := len(c.form.suggest)
	seekCell(t, c, queryRow{field: colCtx}, colMinCell)

	for i := 0; i < len(minStops(colCtx))-1; i++ {
		c.handleKey(keyMsg("space")) // up to the strictest context floor
	}

	assert.NewCollecting(t).Less(before, len(c.form.suggest), "suggestions")
}

func TestEscapeClosesThePanelAndKeepsTheQuery(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	seekCell(t, c, queryRow{field: colIntel}, colSortCell)
	c.handleKey(keyMsg("space"))

	c.handleKey(keyMsg("esc"))

	ck.Nil(c.query, "esc did not close the panel")
	ck.Require().NotNil(c.form, "esc dismissed the form as well")
	var found bool
	for _, k := range c.modelView.keys {
		if k.field == colIntel {
			found = true
		}
	}
	ck.True(found, "the query was discarded when the panel closed")
}

// The selected cell and an active cell are different questions -- where the
// cursor is, and what the query constrains -- and both must be visible.
func TestDialogMarksSelectionAndActivationSeparately(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	c.modelView.setBound(colCtx, bound{minIx: 1})

	out := ansi.Strip(c.query.view(120, 44, c.modelView))
	ck.StrContains(out, "≥128k", "an active bound is not shown:\n")
	ck.StrContains(out, "[", "the selected cell is not marked:\n")
}

func TestSortCellsShowDirectionAndPriority(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	c.modelView.keys = []sortKey{{field: colCtx, desc: true}, {field: colIn}}

	out := ansi.Strip(c.query.view(120, 44, c.modelView))
	ck.StrContains(out, "↓ 1", "primary key missing its arrow and priority:\n")
	ck.StrContains(out, "↑ 2", "secondary key missing its arrow and priority:\n")
}

func TestPanelCostsTheListItsHeight(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), queryFixture())
	focusModelRow(c)
	open := c.form.suggestWindow(44, nil)
	closed := c.form.suggestWindow(44, &queryDialog{})
	assert.NewCollecting(t).Eq(queryHeight(&queryDialog{}, 44), open-closed, "window %d -> %d, want the panel to cost exactly its height", open, closed)
}

func TestPriorityDigitDegradesPastNine(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("3", priorityDigit(3), "priorityDigit(3) =")
	c.Eq("+", priorityDigit(12), "priorityDigit(12)")
}

// Navigation only lands on cells that can hold something, so every available
// cell must have somewhere to cycle to.
func TestEveryAvailableCellHasSomethingToCycle(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, r := range queryRowsList() {
		if r.flag != flagNone {
			continue
		}
		c.False(r.available(colMinCell) && len(minStops(r.field)) < 2, "%v min is navigable but has nothing to cycle", r.field)
		c.False(r.available(colMaxCell) && len(maxStops(r.field)) < 2, "%v max is navigable but has nothing to cycle", r.field)
	}
}

// "off" read as "models with tools off" rather than "this filter is not
// applied", which is the one misreading that matters. The words now name what
// the filter DOES.
func TestCapabilityCellsSayAnyRatherThanOff(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := openQuery(t)
	c.modelView.toolsOnly = false
	c.modelView.visionOnly = true

	out := ansi.Strip(c.query.view(120, 44, c.modelView))
	ck.NotStrContains(out, "off", "a capability cell still reads \"off\":\n")
	ck.StrContains(out, "any", "an unapplied filter should read \"any\":\n")
	ck.StrContains(out, "required", "an applied filter should read \"required\":\n")
}

// "any" must genuinely mean unfiltered: a model KNOWN not to tool-call is
// admitted, which is the whole difference from "required".
func TestAnyMeansUnfilteredNotExcluded(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []*rafikiv1.ModelRow{
		{Id: "a/tools", SupportedParameters: []string{"tools"}},
		{Id: "b/none", SupportedParameters: []string{"temperature"}},
		{Id: "c/unknown"},
	}
	v := defaultModelView()

	v.toolsOnly = false
	c.Eq(3, len(selectModels(rows, "", v)), "with tools=any got")

	v.toolsOnly = true
	got := selectModels(rows, "", v)
	c.Require().Len(got, 2, "with tools=required got %d rows, want 2", len(got))
	// ...and "required" still admits UNKNOWN, or the local fleet disappears.
	var sawUnknown bool
	for _, r := range got {
		if r.GetId() == "c/unknown" {
			sawUnknown = true
		}
	}
	c.True(sawUnknown, "required excluded a model of unknown capability")
}

// ── remembering the query ────────────────────────────────────────────────────

func TestModelViewRoundTripsThroughDisk(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	want := defaultModelView()
	want.keys = []sortKey{{field: colIntel, desc: true}, {field: colIn}}
	want.setBound(colCtx, bound{minIx: stopIndex(t, minStops(colCtx), "1M")})
	want.setBound(colIn, bound{
		minIx: stopIndex(t, minStops(colIn), ">free"),
		maxIx: stopIndex(t, maxStops(colIn), "$2"),
	})
	want.visionOnly = true

	saveModelView("test", want)
	got := loadModelView("test")

	c.False(len(got.keys) != 2 || got.keys[0].field != colIntel || !got.keys[0].desc, "keys = %+v, want intel↓ then in$↑", got.keys)
	if got.keys[1].field != colIn || got.keys[1].desc {
		t.Errorf("second key = %+v, want in$ ascending", got.keys[1])
	}
	c.Eq(want.boundFor(colCtx), got.boundFor(colCtx), "ctx bound")
	c.Eq(want.boundFor(colIn), got.boundFor(colIn), "in$ bound")
	c.False(!got.visionOnly || !got.toolsOnly, "flags = vision:%v tools:%v, want both on", got.visionOnly, got.toolsOnly)
}

// Storing by NAME is what stops a reordered enum silently reinterpreting a
// saved query -- "sort by intelligence" must not become "sort by code".
func TestStoredQueryIsKeyedByNameNotOrdinal(t *testing.T) {
	c := assert.NewCollecting(t)
	v := defaultModelView()
	v.keys = []sortKey{{field: colAgentic, desc: true}}
	v.setBound(colCtx, bound{minIx: 3})

	p := toStored(v)
	c.Eq("agentic", p.Keys[0].Field, "key stored as")
	if _, ok := p.Bounds["ctx"]; !ok {
		t.Errorf("bounds keyed by %v, want the field name", p.Bounds)
	}
	c.Eq("1M", p.Bounds["ctx"].Min, "bound stored as")
}

// An unrecognised name degrades the query rather than refusing it.
func TestUnknownFieldsAndStopsAreDropped(t *testing.T) {
	c := assert.NewCollecting(t)
	v := fromStored(&clientstate.ModelView{
		Keys: []clientstate.SortKey{{Field: "no-such-field"}, {Field: "ctx", Desc: true}},
		Bounds: map[string]clientstate.Bound{
			"no-such-field": {Min: "1M"},
			"ctx":           {Min: "no-such-stop"},
		},
		ToolsOnly: true,
	})
	c.False(len(v.keys) != 1 || v.keys[0].field != colCtx, "keys = %+v, want only the recognised one", v.keys)
	c.False(v.boundFor(colCtx).set(), "an unrecognised stop label produced a bound anyway")
	_, ok := v.bounds[colModel]
	c.False(ok, "an unrecognised field produced a bound")
}

// A document that decodes to nothing orderable still needs a total order, or
// the list comes back in whatever order the daemon happened to send.
func TestEmptyStoredQueryStillSorts(t *testing.T) {
	if v := fromStored(&clientstate.ModelView{}); len(v.keys) == 0 {
		t.Fatal("no sort keys at all")
	}
	v := fromStored(nil)
	assert.NewAborting(t).NotEmpty(v.keys, "a nil section produced no sort keys")
}

// Every failure is silent: a UI preference must never stop the cockpit opening.
func TestCorruptOrMissingStateFallsBackToDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	if got := loadModelView("test"); !got.toolsOnly {
		t.Error("a missing file did not fall back to the default view")
	}

	path := profile.StateFile("test")
	c.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	c.NoError(os.WriteFile(path, []byte("{not json"), 0o600))
	if got := loadModelView("test"); !got.toolsOnly || len(got.keys) == 0 {
		t.Errorf("a corrupt file did not fall back to the default view: %+v", got)
	}
}

// Closing the panel commits, so the next cockpit opens on the same query.
func TestClosingThePanelSavesTheQuery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := openQuery(t)
	// openQuery's cockpit is built with no ProfileName (unreachable in
	// production -- both real NewCockpit call sites always set one), and
	// saveModelView/loadModelView are now no-ops for an empty profile name:
	// writing per-profile state into the GLOBAL document on an unset name
	// would be exactly the leak Scope's own doc comment claims can never
	// happen. Give it a name here so this test exercises the real,
	// production-reachable path.
	c.profileName = "test"
	seekCell(t, c, queryRow{field: colAgentic}, colSortCell)
	c.handleKey(keyMsg("space"))

	c.handleKey(keyMsg("esc"))

	got := loadModelView(c.profileName)
	var found bool
	for _, k := range got.keys {
		if k.field == colAgentic {
			found = true
		}
	}
	assert.NewCollecting(t).True(found, "the query was not persisted when the panel closed")
}

// TestModelViewStoreIsANoOpForAnEmptyProfileName pins Fix 8: an empty
// profileName must never read or write the GLOBAL clientstate document.
// saveModelView must no-op, and loadModelView must fall back to the default
// view rather than whatever (unrelated) data happens to live in the global
// document's ModelView section.
func TestModelViewStoreIsANoOpForAnEmptyProfileName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	v := defaultModelView()
	v.keys = []sortKey{{field: colAgentic, desc: true}}
	saveModelView("", v)

	got := loadModelView("")
	for _, k := range got.keys {
		assert.NewAborting(t).NotEq(colAgentic, k.field, "saveModelView(\"\", ...) persisted state despite the empty profile name")
	}
}

// TestScoredStopExcludesUnscoredModels pins the ONE exception to the
// admit-unknowns rule.
//
// This stop shipped as a no-op: the old admits() tested "value absent" before
// it reached the scored branch, so selecting it filtered nothing while the
// panel displayed a constraint. Nothing caught it because no test built a
// bound from this stop. A numeric minimum cannot substitute -- "intel >= 55"
// admits unscored rows deliberately, which
// TestBoundsAdmitModelsTheCatalogCannotAnswerFor pins -- so this is the only
// way to ask for benchmarked models only.
func TestScoredStopExcludesUnscoredModels(t *testing.T) {
	c := assert.NewCollecting(t)
	v := defaultModelView()
	v.setBound(colIntel, bound{minIx: stopIndex(t, minStops(colIntel), "scored")})

	rows := selectModels(scoredQueryRows(), "", v)
	c.Require().NotEmpty(rows, "the scored stop excluded everything, including scored models")
	for _, r := range rows {
		c.NotNil(r.IntelligenceIndex, "row %q has no intelligence score but survived the scored stop; "+
			"the stop is a no-op again", r.GetId())
	}
}

// TestScoredStopIsNotAppliedToOtherFields guards the blast radius: the
// exception belongs to the score fields that declare it, and must not leak
// into a price or context bound, where rejecting unknowns would hide every
// locally-served model.
func TestScoredStopIsNotAppliedToOtherFields(t *testing.T) {
	v := defaultModelView()
	v.setBound(colCtx, bound{minIx: stopIndex(t, minStops(colCtx), "128k")})

	var found bool
	for _, r := range selectModels(scoredQueryRows(), "", v) {
		if r.GetId() == "local/unknown" {
			found = true
		}
	}
	assert.NewCollecting(t).True(found, "a context bound rejected a model the catalog cannot answer for")
}

func scoredQueryRows() []*rafikiv1.ModelRow {
	return []*rafikiv1.ModelRow{
		{Id: "or/scored-high", IntelligenceIndex: f64q(61.0), ContextWindow: i32q(200_000)},
		{Id: "or/scored-low", IntelligenceIndex: f64q(30.0), ContextWindow: i32q(200_000)},
		{Id: "or/unscored", ContextWindow: i32q(200_000)},
		{Id: "local/unknown"}, // no catalog facts at all
	}
}
