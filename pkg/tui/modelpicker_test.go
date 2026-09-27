// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func i32p(v int32) *int32   { return &v }
func fp(v float64) *float64 { return &v }

// modelRows: a priced vision model, a priced text-only model, and a local
// model the catalog has never heard of.
func modelRows() []*rafikiv1.ModelRow {
	return []*rafikiv1.ModelRow{
		{Id: "openai/gpt-4o", ContextWindow: i32p(128000), PromptUsd: fp(0.000005),
			CompletionUsd: fp(0.000015), InputModalities: []string{"text", "image"}},
		{Id: "deepseek/chat", ContextWindow: i32p(64000), PromptUsd: fp(0.0000002),
			CompletionUsd: fp(0.0000006), InputModalities: []string{"text"}},
		{Id: "ollama/llama3"}, // no catalog entry at all
	}
}

// seedModels installs a catalog the way a completed fetch would.
func seedModels(c *Cockpit, kind string, rows []*rafikiv1.ModelRow) {
	c.applyModelsLoaded(modelsLoadedMsg{kind: kind, rows: rows})
}

// focusModelRow moves to the model row the way a user does.
//
// Setting form.focus directly is NOT equivalent: moveFocus is what calls
// Focus() on the input, and a blurred bubbles input silently swallows every
// key. A test that skips it passes or fails for the wrong reason.
func focusModelRow(c *Cockpit) {
	for c.form.focus != fieldModel {
		c.form.moveFocus(+1)
	}
}

func loadedPicker(t *testing.T) (*Cockpit, *modelPicker) {
	t.Helper()
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	assert.NewAborting(t).NotNil(c.picker, "^F on the model row did not open the picker")
	return c, c.picker
}

func TestCtrlFOpensTheFullBrowser(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	ck.Require().NotNil(c.picker, "no picker opened")
	ck.NotNil(c.form, "the form was dismissed; the picker stacks on top of it")
}

// A cached catalog means the browser opens with rows already in it -- no
// second round trip for what the typeahead already fetched.
func TestPickerOpensFromTheCacheWithoutRefetching(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	ck.False(c.picker.loading, "picker opened in a loading state despite a warm cache")
	ck.Len(c.picker.rows, 3, "rows = %d, want the cached 3", len(c.picker.rows))
	// The guard is on fetchModelsCmd itself, so callers can issue it on every
	// event that might need models without tracking state.
	ck.Nil(c.fetchModelsCmd(c.form.kind()), "a refetch was issued for a kind already in the cache")
}

// An in-flight fetch must not be started twice: the form opening and the model
// row being reached are two events for the same catalog.
func TestFetchIsNotIssuedTwiceForOneKind(t *testing.T) {
	ck := assert.NewCollecting(t)
	// A bare cockpit, not formCockpit: opening the form already prefetches,
	// which is itself the guard working.
	c := newTestCockpit("")
	ck.Require().NotNil(c.fetchModelsCmd("fundi"), "no fetch issued for a cold cache")
	ck.Nil(c.fetchModelsCmd("fundi"), "a second fetch was issued while the first was in flight")
}

// Typed text is a head start, not discarded work.
func TestTypedModelTextSeedsTheFilter(t *testing.T) {
	c := formCockpit(t)
	focusModelRow(c)
	c.form.inputs[fieldModel].SetValue("gpt")
	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	assert.NewCollecting(t).Eq("gpt", c.picker.filter.Value(), "filter")
}

func TestFilterNarrowsTheRows(t *testing.T) {
	c, p := loadedPicker(t)
	assert.NewAborting(t).Len(p.rows, 3, "rows = %d, want all 3 before filtering", len(p.rows))
	p.filter.SetValue("deep")
	p.apply(c.modelView)

	if len(p.rows) != 1 || p.rows[0].GetId() != "deepseek/chat" {
		t.Errorf("rows = %v, want just deepseek/chat", p.rows)
	}
}

// Changing the filter must reset the cursor: leaving it where it was selects
// whatever happens to land under it.
func TestFilterResetsTheCursor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	p.move(+2, 10)
	ck.Require().NotEq(0, p.cursor, "cursor did not move")
	c.handleKey(tea.KeyPressMsg{Code: 'o', Text: "o"})

	ck.Eq(0, p.cursor, "cursor")
}

// ── the presence rules ───────────────────────────────────────────────────────

// An unpriced model is not the cheapest thing available. Sorting an absent
// price as zero is exactly what the optional wire fields exist to prevent.
func TestCheapestSortPutsUnpricedModelsLast(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	c.modelView.keys = []sortKey{{field: colIn}}
	p.apply(c.modelView)

	ck.Eq("deepseek/chat", p.rows[0].GetId(), "first row")
	ck.Eq("ollama/llama3", p.rows[len(p.rows)-1].GetId(), "last row")
}

func TestBiggestContextSortPutsUnknownContextLast(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	c.modelView.keys = []sortKey{{field: colCtx, desc: true}}
	p.apply(c.modelView)

	ck.Eq("openai/gpt-4o", p.rows[0].GetId(), "first row")
	ck.Eq("ollama/llama3", p.rows[len(p.rows)-1].GetId(), "last row")
}

// The trap the whole design warns about: empty modalities means the daemon has
// NO catalog entry, not "no vision". A filter that dropped them would hide
// every locally-served model.
func TestVisionFilterKeepsUnknownsAndDropsOnlyKnownTextOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	c.modelView.visionOnly = true
	p.apply(c.modelView)

	ids := map[string]bool{}
	for _, r := range p.rows {
		ids[r.GetId()] = true
	}
	ck.False(!ids["openai/gpt-4o"], "vision filter dropped a model that HAS vision")
	ck.False(ids["deepseek/chat"], "vision filter kept a model known to be text-only")
	ck.Require().False(!ids["ollama/llama3"], "vision filter dropped an UNKNOWN model; that hides the whole local fleet")
}

// Keeping unknowns is only honest if the user is told. The count is the thing
// that says the ◉ column is not the whole answer.
func TestFooterCountsUnknownCapability(t *testing.T) {
	c, p := loadedPicker(t)
	assert.NewCollecting(t).StrContains(p.footer(c.modelView), "1 unknown", "footer")
}

func TestAbsentFactsRenderAsDashesNotZeros(t *testing.T) {
	c := assert.NewCollecting(t)
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	c.Eq("—", ctxCell(bare), "ctxCell")
	c.Eq("—", priceCell(bare.PromptUsd), "priceCell")
	c.Eq("?", visionCellGlyph(bare), "visionCellGlyph")
}

// A model priced at zero is genuinely free and must not render as unknown.
func TestZeroPriceRendersAsZeroNotUnknown(t *testing.T) {
	assert.NewCollecting(t).Eq("0.00", priceCell(fp(0)), "priceCell(0)")
}

// ── selection ────────────────────────────────────────────────────────────────

func TestPickingFillsTheFieldAndReturnsToTheForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	p.filter.SetValue("gpt")
	p.apply(c.modelView)

	c.handleKey(keyMsg("enter"))

	ck.Nil(c.picker, "picker stayed open after a pick")
	ck.Eq("openai/gpt-4o", c.form.inputs[fieldModel].Value(), "model field")
	// Focus advances so the next ⏎ submits rather than reopening the picker.
	ck.NotEq(fieldModel, c.form.focus, "focus stayed on the model row; ⏎ would reopen the picker")
}

// esc returns to the FORM, not out of both: the other fields are half filled.
func TestEscapeReturnsToTheForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := loadedPicker(t)
	c.handleKey(keyMsg("esc"))

	ck.Nil(c.picker, "esc did not close the picker")
	ck.Require().NotNil(c.form, "esc dismissed the form too; the half-filled fields are gone")
}

func TestPickingNothingLeavesTheFieldAlone(t *testing.T) {
	c, p := loadedPicker(t)
	c.form.inputs[fieldModel].SetValue("typed/by-hand")
	p.filter.SetValue("no-such-model")
	p.apply(c.modelView)

	c.handleKey(keyMsg("enter"))

	assert.NewCollecting(t).Eq("typed/by-hand", c.form.inputs[fieldModel].Value(), "model field")
}

// ── failure ──────────────────────────────────────────────────────────────────

// A daemon that cannot answer must not trap the user: the field still accepts
// a hand-typed id.
func TestFetchFailureIsReportedAndRecoverable(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.applyModelsLoaded(modelsLoadedMsg{kind: c.form.kind(),
		err: errors.New("unavailable: model lister not yet wired")})
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	c.width, c.height, c.ready = 100, 30, true
	out := ansi.Strip(c.View().Content)
	ck.StrContains(out, "not yet wired", "view did not report the failure:\n")
	ck.StrContains(out, "by hand", "view did not say the id can still be typed by hand")

	c.handleKey(keyMsg("esc"))
	ck.Require().NotNil(c.form, "esc after a failure dismissed the form")
}

// A late answer for a kind the form no longer has must be dropped, not shown.
func TestStaleFetchIsIgnored(t *testing.T) {
	c, _ := loadedPicker(t)
	c.applyModelsLoaded(modelsLoadedMsg{kind: "some-other-kind",
		rows: []*rafikiv1.ModelRow{{Id: "wrong/model"}}})

	for _, r := range c.picker.rows {
		assert.NewAborting(t).NotEq("wrong/model", r.GetId(), "a stale fetch for another kind was applied")
	}
}

func TestPickerOwnsTheBodyPaneAboveTheForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := loadedPicker(t)
	c.width, c.height, c.ready = 100, 30, true

	out := ansi.Strip(c.View().Content)
	ck.StrContains(out, "openai/gpt-4o", "the picker did not render")
	ck.NotStrContains(out, "new agent", "the form rendered over the picker")
}

// ^R still cycles the primary key; ^S now opens the dialog instead.
func TestCtrlRCyclesThePrimarySortAndWraps(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := loadedPicker(t)
	first := c.modelView.keys[0].field
	seen := map[modelField]bool{first: true}
	for i := 0; i < int(modelFieldCount)-1; i++ {
		c.handleKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
		got := c.modelView.keys[0].field
		ck.Require().False(seen[got], "^R revisited %v before covering every field", got)
		seen[got] = true
	}
	c.handleKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	ck.Eq(first, c.modelView.keys[0].field, "field")
}

// Every field must have a label: an unnamed one renders "?" in the dialog,
// which is the only place the ordering is visible.
func TestEveryFieldIsNamed(t *testing.T) {
	for f := modelField(0); f < modelFieldCount; f++ {
		assert.NewCollecting(t).NotEq("?", f.String(), "field %d has no label", f)
	}
}

// The point of the whole interaction: filtering happens on the keystroke, with
// no key to press to make it happen.
func TestTypingFiltersLiveWithNoConfirmingKey(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	for _, r := range "deep" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if len(c.form.suggest) != 1 || c.form.suggest[0].GetId() != "deepseek/chat" {
		t.Fatalf("suggest = %v, want just deepseek/chat with no key pressed", c.form.suggest)
	}
}

// Opening the form must prefetch: a round trip started on the first keystroke
// is not "live".
func TestOpeningTheFormPrefetchesTheCatalog(t *testing.T) {
	c := railWith(t, "c_1")
	c.handleKey(keyMsg("n"))

	assert.NewCollecting(t).False(!c.modelsBusy["fundi"], "no catalog fetch was started when the form opened")
}

// An empty model field still lists, so ↓ browses. An empty box that answers
// nothing looks broken.
func TestEmptyModelFieldStillSuggests(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	assert.NewCollecting(t).True(c.form.showSuggestions(), "no suggestions for an empty field; ↓ would have nothing to browse")
}

// The list follows FOCUS: tabbing away must not leave it floating under a
// field nobody is editing.
func TestSuggestionsHideWhenTheModelRowLosesFocus(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	ck.Require().True(c.form.showSuggestions(), "no suggestions to begin with")

	c.handleKey(keyMsg("tab"))

	ck.False(c.form.showSuggestions(), "suggestions still showing after the model row lost focus")
}

// ↓ on the model row walks INTO the list rather than to the next field.
func TestDownEntersTheSuggestionList(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(keyMsg("down"))

	ck.Require().Eq(fieldModel, c.form.focus, "↓ left the model row instead of entering the list")
	ck.Eq(0, c.form.suggestCur, "suggestCur")
}

// ↑ off the top of the list returns to the text, not to the previous field.
func TestUpOffTheListReturnsToTheText(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))

	c.handleKey(keyMsg("up"))

	ck.Eq(-1, c.form.suggestCur, "suggestCur")
	ck.Eq(fieldModel, c.form.focus, "↑ left the model row; the way out of a typeahead is back to the text")
}

// ↑ from the SECOND row lands on the first, not back in the text. Clamping
// the decrement at 0 and then treating 0 as "leave the list" skips the top row
// entirely, which makes the first suggestion unreachable with the keyboard.
func TestUpFromTheSecondRowLandsOnTheFirst(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	c.handleKey(keyMsg("down"))
	ck.Require().Eq(1, c.form.suggestCur, "suggestCur")

	c.handleKey(keyMsg("up"))

	ck.Eq(0, c.form.suggestCur, "suggestCur")
}

// ⏎ takes the highlighted suggestion.
func TestEnterTakesTheHighlightedSuggestion(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.Require().NotEq("", c.form.inputs[fieldModel].Value(), "⏎ on a highlighted suggestion filled nothing")
	ck.Nil(cmd, "⏎ on a suggestion also submitted the form")
}

// ...and with NOTHING highlighted it submits, exactly as on every other row.
// This is what stops ⏎ meaning two things at the same moment.
func TestEnterWithNoHighlightSubmits(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	ck.Require().Eq(-1, c.form.suggestCur, "something was highlighted before any ↓")

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.NotNil(cmd, "⏎ with no highlight did not submit")
}

// Retyping must drop the highlight: a cursor left on row 3 of the OLD list
// selects whatever now happens to sit there.
func TestTypingClearsTheHighlight(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	ck.Require().Eq(0, c.form.suggestCur, "nothing highlighted to begin with")

	c.handleKey(tea.KeyPressMsg{Code: 'o', Text: "o"})

	ck.Eq(-1, c.form.suggestCur, "suggestCur")
}

// The two kinds have different model universes, so cycling kind must rebuild
// the list from the other catalog rather than leave the old one showing.
func TestCyclingKindRebuildsTheSuggestions(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	seedModels(c, protocol.KindFundi, modelRows())
	seedModels(c, protocol.KindClaude, []*rafikiv1.ModelRow{
		{Id: "anthropic/claude-opus-5", ContextWindow: i32p(200000)},
	})
	focusModelRow(c)
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
	ck.Len(c.form.suggest, 3, "suggest = %d, want the 3 fundi rows", len(c.form.suggest))

	c.form.focus = fieldKind
	c.handleKey(keyMsg("right"))

	ck.Eq(protocol.KindClaude, c.form.kind(), "kind")
	if len(c.form.suggest) != 1 || c.form.suggest[0].GetId() != "anthropic/claude-opus-5" {
		t.Errorf("suggest = %v, want the claude catalog", c.form.suggest)
	}
}

// The list fills the panel rather than a fixed handful, and holds EVERY match
// so a filter hitting 40 models is navigable instead of silently truncated.
func TestSuggestionsFillThePanelAndKeepEveryMatch(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	many := make([]*rafikiv1.ModelRow, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, &rafikiv1.ModelRow{Id: fmt.Sprintf("x/model-%02d", i)})
	}
	seedModels(c, c.form.kind(), many)
	focusModelRow(c)

	ck.Len(c.form.suggest, 40, "suggest = %d, want every match retained", len(c.form.suggest))
	// A tall pane shows more rows than a short one; that is the whole request.
	tall := c.form.suggestWindow(40, nil)
	short := c.form.suggestWindow(14, nil)
	ck.Greater(short, tall, "window: tall")
	// The view renders the window PLUS the fixed-height detail block.
	got, want := strings.Count(c.form.suggestView(90, tall, c.modelView), "\n"), tall+detailHeight
	ck.Eq(want, got, "rendered %d rows, want %d (window %d + detail %d)", got, want, tall, detailHeight)
}

// Walking past the bottom of the window scrolls rather than stopping.
func TestSuggestionListScrolls(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	many := make([]*rafikiv1.ModelRow, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, &rafikiv1.ModelRow{Id: fmt.Sprintf("x/model-%02d", i)})
	}
	seedModels(c, c.form.kind(), many)
	focusModelRow(c)

	window := 5
	for i := 0; i < 12; i++ {
		c.form.moveSuggest(+1, window)
	}
	ck.Require().Eq(11, c.form.suggestCur, "suggestCur")
	ck.NotEq(0, c.form.suggestOff, "the window never scrolled; rows past the first screenful are unreachable")
	if c.form.suggestCur < c.form.suggestOff ||
		c.form.suggestCur >= c.form.suggestOff+window {
		t.Errorf("cursor %d outside the drawn window [%d,%d)",
			c.form.suggestCur, c.form.suggestOff, c.form.suggestOff+window)
	}
}

// A new filter restarts the window, not just the highlight: scrolled deep into
// the old list, the new one would otherwise open somewhere arbitrary.
func TestFilteringResetsTheScrollWindow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	many := make([]*rafikiv1.ModelRow, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, &rafikiv1.ModelRow{Id: fmt.Sprintf("x/model-%02d", i)})
	}
	seedModels(c, c.form.kind(), many)
	focusModelRow(c)
	for i := 0; i < 20; i++ {
		c.form.moveSuggest(+1, 5)
	}
	ck.Require().NotEq(0, c.form.suggestOff, "did not scroll")

	c.handleKey(tea.KeyPressMsg{Code: '3', Text: "3"})

	ck.Eq(0, c.form.suggestOff, "suggestOff")
}

// Each suggestion carries the facts that decide the choice; the id alone does
// not answer "which of these three opus ids".
func TestSuggestionsShowTheDecidingFacts(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	out := ansi.Strip(c.form.suggestView(90, 10, c.modelView))
	ck.StrContains(out, "128k", "no context column in the typeahead")
	ck.StrContains(out, "5.00", "no price column in the typeahead")
	ck.StrContains(out, "?", "the unknown-capability model does not render as unknown")
}

// ── sort and vision, shared by both views ────────────────────────────────────

// Sorting reaches the inline typeahead, not only the full browser.
func TestSortReachesTheInlineTypeahead(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	ck.Require().Eq("deepseek/chat", c.form.suggest[0].GetId(), "first suggestion")

	c.modelView.keys = []sortKey{{field: colIn}}
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)

	ck.Eq("deepseek/chat", c.form.suggest[0].GetId(), "first suggestion")
	ck.Eq("ollama/llama3", c.form.suggest[len(c.form.suggest)-1].GetId(), "last suggestion")
}

func TestCtrlVFiltersVisionInTheInlineTypeahead(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})

	ids := map[string]bool{}
	for _, r := range c.form.suggest {
		ids[r.GetId()] = true
	}
	ck.False(ids["deepseek/chat"], "a model known to be text-only survived the vision filter")
	ck.False(!ids["ollama/llama3"], "an UNKNOWN-capability model was dropped; that hides the local fleet")
}

// One setting, two windows: sorting inline then opening the browser must not
// silently reorder under you.
func TestSortCarriesFromTheTypeaheadIntoTheBrowser(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.modelView.keys = []sortKey{{field: colIn}}
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	ck.Require().NotNil(c.picker, "browser did not open")
	ck.Eq("ollama/llama3", c.picker.rows[len(c.picker.rows)-1].GetId(), "the browser opened in a different order than the typeahead")
}

func TestSortCarriesFromTheBrowserBackToTheTypeahead(t *testing.T) {
	c, _ := loadedPicker(t)
	c.handleKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	want := c.modelView.keys[0].field

	c.handleKey(keyMsg("esc")) // back to the form
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)

	assert.NewCollecting(t).Eq(want, c.modelView.keys[0].field, "field")
}

// The two views must never disagree about what matches.
func TestBothViewsSelectIdenticallyForTheSameQuery(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.modelView = modelView{keys: []sortKey{{field: colIn}}, visionOnly: true}

	c.form.inputs[fieldModel].SetValue("a")
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
	inline := c.form.suggest

	p := newModelPicker(c.form.kind(), "a", c.models[c.form.kind()], true, "", c.modelView)

	ck.Require().Len(inline, len(p.rows), "typeahead %d rows, browser %d — the two disagree", len(inline), len(p.rows))
	for i := range inline {
		ck.Eq(p.rows[i].GetId(), inline[i].GetId(), "row %d: typeahead %q, browser", i, inline[i].GetId())
	}
}

func TestHintLineNamesTheActiveView(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.modelView = modelView{keys: []sortKey{{field: colIn}}, visionOnly: true}

	out := ansi.Strip(c.form.view(90, 24, c.modelView, nil, nil))
	ck.StrContains(out, "in$", "hint line does not name the sort:\n")
	ck.StrContains(out, "vision required", "hint line does not say the vision filter is on:\n")
}

// Changing the query must drop the highlight: it names a row in the OLD
// order, and keeping it selects whatever now sits there.
func TestChangingTheViewClearsTheHighlight(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	ck.Require().Eq(0, c.form.suggestCur, "nothing highlighted to begin with")

	c.handleKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})

	ck.Eq(-1, c.form.suggestCur, "suggestCur")
}

func toolRows() []*rafikiv1.ModelRow {
	day := int64(24 * 60 * 60)
	now := time.Now().Unix()
	return []*rafikiv1.ModelRow{
		{Id: "a/agentic", Created: &[]int64{now - 5*day}[0],
			SupportedParameters: []string{"tools", "reasoning"}},
		{Id: "b/chat-only", Created: &[]int64{now - 400*day}[0],
			SupportedParameters: []string{"temperature"}},
		{Id: "c/unknown"}, // no catalog entry at all
	}
}

// The default: a model that cannot tool-call is not a candidate for an agent,
// so it is hidden without being asked.
func TestToolsFilterIsOnByDefault(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	ck.Require().True(c.modelView.toolsOnly, "toolsOnly defaults off; a non-agentic model would be offered")
	seedModels(c, c.form.kind(), toolRows())
	focusModelRow(c)

	ids := map[string]bool{}
	for _, r := range c.form.suggest {
		ids[r.GetId()] = true
	}
	ck.False(ids["b/chat-only"], "a model known not to support tools was offered by default")
	ck.False(!ids["a/agentic"], "a tool-capable model was hidden")
	// The same trap as vision: nil means no catalog entry, which is every
	// locally-served model. Reading it as "no tools" hides the local fleet.
	ck.Require().False(!ids["c/unknown"], "an UNKNOWN-capability model was hidden by the default filter")
}

func TestCtrlTRevealsNonToolModels(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), toolRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})

	ck.Require().False(c.modelView.toolsOnly, "^T did not toggle the filter")
	found := false
	for _, r := range c.form.suggest {
		if r.GetId() == "b/chat-only" {
			found = true
		}
	}
	ck.True(found, "^T did not reveal the non-tool model")
}

// Off is the notable state, because on is the default: a list silently
// including models that cannot be agents is the surprising one.
func TestHintLineFlagsWhenNonToolModelsAreIncluded(t *testing.T) {
	c := assert.NewCollecting(t)
	v := defaultModelView()
	c.NotStrContains(v.summary(), "tools any", "the default view advertises a filter that is simply on")
	v.toggleTools()
	c.StrContains(v.summary(), "tools any", "summary")
}

func TestNewestSortOrdersByListingDateAndPutsUnknownLast(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	c.modelView = modelView{keys: []sortKey{{field: colAge, desc: true}}}
	p.all = toolRows()
	p.apply(c.modelView)

	ck.Eq("a/agentic", p.rows[0].GetId(), "first row")
	ck.Eq("c/unknown", p.rows[len(p.rows)-1].GetId(), "last row")
}

// Sorting by something invisible is a list that reorders for no visible
// reason, so an unpinned sort field brings its own column.
func TestOnlyUnpinnedFieldsAddAColumn(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, f := range []modelField{colModel, colCtx, colIn, colOut} {
		c.Empty(extraColumns([]sortKey{{field: f}}), "sorting by %v added a column; it is already pinned", f)
	}
	got := extraColumns([]sortKey{{field: colAge, desc: true}})
	c.False(len(got) != 1 || got[0] != colAge, "extraColumns = %v, want [age]", got)
	if title, w := headerFor(colAge); title != "AGE" || w <= 0 {
		t.Errorf("headerFor(colAge) = (%q,%d), want an AGE column", title, w)
	}
}

// Two keys may add two columns, no more: a four-key sort must not squeeze the
// model id off the row.
func TestExtraColumnsAreCappedAtTwo(t *testing.T) {
	keys := []sortKey{{field: colAge}, {field: colIntel}, {field: colCode}, {field: colAgentic}}
	assert.NewCollecting(t).Len(extraColumns(keys), 2, "extraColumns")
}

func TestAgeCellIsCoarseAndAbsenceIsADash(t *testing.T) {
	c := assert.NewCollecting(t)
	now := time.Now()
	day := int64(24 * 60 * 60)
	mk := func(off int64) *rafikiv1.ModelRow {
		v := now.Unix() - off
		return &rafikiv1.ModelRow{Created: &v}
	}
	for _, tc := range []struct {
		off  int64
		want string
	}{
		{0, "today"}, {5 * day, "5d"}, {90 * day, "3mo"}, {800 * day, "2.2y"},
	} {
		got := ageCell(mk(tc.off), now)
		c.Eq(tc.want, got, "ageCell(%dd) = %q, want", tc.off/day, got)
	}
	c.Eq("—", ageCell(&rafikiv1.ModelRow{}, now), "ageCell(absent)")
}

// Expiry is a forward warning, and the far-future sentinel is not one.
func TestExpiryWarnsOnlyWithinAYear(t *testing.T) {
	c := assert.NewCollecting(t)
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	soon := &rafikiv1.ModelRow{ExpiresAt: "2026-09-08"}
	if got := expiryWarning(soon, now); !strings.Contains(got, "6d") ||
		!strings.Contains(got, "2026-09-08") {
		t.Errorf("expiryWarning(soon) = %q, want the date and the countdown", got)
	}
	// "2098-12-31" means "no planned removal"; warning on it would put a
	// notice next to models in no danger at all.
	sentinel := &rafikiv1.ModelRow{ExpiresAt: "2098-12-31"}
	c.Eq("", expiryWarning(sentinel, now), "expiryWarning(sentinel)")
	c.Eq("", expiryWarning(&rafikiv1.ModelRow{}, now), "expiryWarning(none)")
	c.Eq("", expiryWarning(&rafikiv1.ModelRow{ExpiresAt: "not-a-date"}, now), "expiryWarning(garbage)")
}

// The detail block is where the sparse facts live, so they cost width on one
// row rather than on every row.
func TestDetailBlockDescribesTheHighlightedRow(t *testing.T) {
	c := formCockpit(t)
	c.modelView.toolsOnly = false
	seedModels(c, c.form.kind(), toolRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))

	out := ansi.Strip(c.form.suggestView(100, 6, c.modelView))
	assert.NewCollecting(t).StrContains(out, "thinking yes", "detail does not report reasoning support:\n")
}

// Fixed position is the whole point: every label is present whether or not it
// has a value, so the eye returns to the same column for the same fact.
func TestDetailBlockLabelsEveryFieldEvenWhenAbsent(t *testing.T) {
	c := assert.NewCollecting(t)
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	lines := modelDetail(bare, time.Now(), 140)
	c.Require().Len(lines, detailHeight, "detail is %d lines, want a fixed", len(lines))
	body := ansi.Strip(lines[1] + " " + lines[2])
	for _, label := range []string{"source", "age", "ctx", "max out", "in/out",
		"cache", "tools", "vision", "thinking"} {
		c.StrContains(body, label, "label")
	}
	c.GreaterOrEqual(4, strings.Count(body, "—"), "absent values should read as em dashes:\n%s", body)
}

// The block keeps its height with nothing highlighted, so the list above it
// does not grow and shrink as the cursor moves.
func TestDetailBlockKeepsItsHeightWhenEmpty(t *testing.T) {
	assert.NewCollecting(t).Eq(detailHeight, len(modelDetail(nil, time.Now(), 80)), "empty detail is")
}

// A rule separates the block from the list; without it the two read as one.
func TestDetailBlockIsSeparatedFromTheList(t *testing.T) {
	lines := modelDetail(&rafikiv1.ModelRow{Id: "a/b"}, time.Now(), 40)
	assert.NewCollecting(t).StrContains(ansi.Strip(lines[0]), "───", "no rule above the detail block")
}

// "unknown" is a real answer and must never render as "no": the daemon has no
// catalog entry for any locally-served model.
func TestDetailBlockSpellsUnknownRatherThanNo(t *testing.T) {
	c := assert.NewCollecting(t)
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	body := ansi.Strip(strings.Join(modelDetail(bare, time.Now(), 140), " "))
	c.StrContains(body, "tools unknown", "tools rendered as something other than unknown:\n")
	c.StrContains(body, "vision unknown", "vision rendered as something other than unknown:\n")
}

// A no-tools model reachable only via ^T must be labelled where it is picked.
func TestDetailBlockFlagsANoToolsModel(t *testing.T) {
	row := &rafikiv1.ModelRow{Id: "b/chat-only", SupportedParameters: []string{"temperature"}}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	assert.NewCollecting(t).StrContains(body, "tools NO", "a model that cannot tool-call is not flagged:\n")
}

// The expiry warning rides the block rather than a column of its own.
func TestDetailBlockCarriesTheExpiryWarning(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	row := &rafikiv1.ModelRow{Id: "a/b", ExpiresAt: "2026-09-08"}
	body := ansi.Strip(strings.Join(modelDetail(row, now, 140), " "))
	assert.NewCollecting(t).StrContains(body, "removed 2026-09-08 (6d)", "no expiry warning in the detail block:\n")
}

// Every value must FIT its cell. A width that clips "unknown" to "unkno…" is
// worse than the free-form line this block replaced, and only a rendered
// check catches it -- the fields are all present either way.
func TestDetailBlockCellsAreWideEnoughForTheirValues(t *testing.T) {
	c := assert.NewCollecting(t)
	i32 := func(v int32) *int32 { return &v }
	f := func(v float64) *float64 { return &v }
	worst := &rafikiv1.ModelRow{
		Id: "x/y", Source: "openrouter",
		ContextWindow: i32(1000000), MaxCompletionTokens: i32(128000),
		PromptUsd: f(0.00001), CompletionUsd: f(0.0001),
		CacheReadUsd: f(0.000001), CacheWriteUsd: f(0.0000125),
		KnowledgeCutoff: "2026-02-16", AgenticIndex: f(100.0),
		IntelligenceIndex: f(100.0), CodingIndex: f(100.0),
		// no supported_parameters and no modalities: both read "unknown",
		// which are the longest values these cells ever hold.
	}
	body := ansi.Strip(strings.Join(modelDetail(worst, time.Now(), 130), " "))
	c.NotStrContains(body, "…", "a detail cell clipped its own value:\n")
	for _, want := range []string{"tools unknown", "vision unknown",
		"source openrouter", "ctx 1.0M", "max out 128k", "in/out 10.00/100.00",
		"cutoff 2026-02-16", "agentic 100.0", "thinking no",
		"intel 100.0", "code 100.0"} {
		c.StrContains(body, want, "%q missing or clipped:\n", want)
	}
}

// ── agentic score and knowledge cutoff ───────────────────────────────────────

func scoredRows() []*rafikiv1.ModelRow {
	f := func(v float64) *float64 { return &v }
	return []*rafikiv1.ModelRow{
		{Id: "a/mid", AgenticIndex: f(40.0), KnowledgeCutoff: "2025-06-30"},
		{Id: "b/best", AgenticIndex: f(59.2), KnowledgeCutoff: "2026-02-16"},
		{Id: "c/unscored"}, // 62% of the live catalog looks like this
	}
}

func TestAgenticSortIsHighestFirstAndUnscoredLast(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	c.modelView = modelView{keys: []sortKey{{field: colAgentic, desc: true}}}
	p.all = scoredRows()
	p.apply(c.modelView)

	ck.Eq("b/best", p.rows[0].GetId(), "first row")
	// Absent is UNSCORED, never zero, and DESCENDING must not flip that: an
	// unscored model at the top of "smartest" is the failure this guards.
	ck.Eq("c/unscored", p.rows[len(p.rows)-1].GetId(), "last row")
}

func TestAgenticSortShowsItsOwnColumn(t *testing.T) {
	c := assert.NewCollecting(t)
	got := extraColumns([]sortKey{{field: colAgentic, desc: true}})
	c.Require().False(len(got) != 1 || got[0] != colAgentic, "extraColumns = %v, want [agentic]", got)
	if title, w := headerFor(colAgentic); title != "AGENTIC" || w <= 0 {
		t.Errorf("headerFor(colAgentic) = (%q,%d), want an AGENTIC column", title, w)
	}
	f := 59.2
	row := &rafikiv1.ModelRow{Id: "b/best", AgenticIndex: &f}
	c.Eq("59.2", cellFor(row, colAgentic, time.Now()), "cellFor")
	c.Eq("—", cellFor(row, colAge, time.Now()), "cellFor(age)")
}

func TestUnscoredAndUncutModelsReadAsAbsentNotZero(t *testing.T) {
	c := assert.NewCollecting(t)
	bare := &rafikiv1.ModelRow{Id: "c/unscored"}
	c.Eq("—", agenticCell(bare), "agenticCell")
	c.Eq("—", cutoffCell(bare), "cutoffCell")
	// A genuinely low score is a real value and must not read as absent.
	low := 0.3
	c.Eq("0.3", agenticCell(&rafikiv1.ModelRow{AgenticIndex: &low}), "agenticCell(0.3)")
}

func TestDetailBlockCarriesCutoffAndAgenticScore(t *testing.T) {
	c := assert.NewCollecting(t)
	f := 59.2
	row := &rafikiv1.ModelRow{Id: "b/best", AgenticIndex: &f, KnowledgeCutoff: "2026-02-16"}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	c.StrContains(body, "agentic 59.2", "no agentic score in the detail block:\n")
	c.StrContains(body, "cutoff 2026-02-16", "no knowledge cutoff in the detail block:\n")
}

// Cutoff and age are different axes and both earn a slot: a model listed last
// week can have a cutoff from a year before that.
func TestCutoffAndAgeAreSeparateFields(t *testing.T) {
	c := assert.NewCollecting(t)
	created := time.Now().AddDate(0, 0, -7).Unix()
	row := &rafikiv1.ModelRow{Id: "x/y", Created: &created, KnowledgeCutoff: "2025-01-31"}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	c.StrContains(body, "age 7d", "age missing or wrong:\n")
	c.StrContains(body, "cutoff 2025-01-31", "cutoff missing:\n")
}

// All three artificial_analysis scores are shown, because they are only
// meaningful read against each other: a high coding score beside a low agentic
// one is the useful shape, and it is invisible if you only render one.
func TestDetailBlockShowsAllThreeBenchmarkScores(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	row := &rafikiv1.ModelRow{Id: "a/b",
		IntelligenceIndex: f(65.7), CodingIndex: f(81.6), AgenticIndex: f(59.2)}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 160), " "))
	for _, want := range []string{"intel 65.7", "code 81.6", "agentic 59.2"} {
		assert.NewCollecting(t).StrContains(body, want, "%q missing from the detail block:\n", want)
	}
}

// They arrive and go missing together, and all three must read as unscored
// rather than as zeros.
func TestUnbenchmarkedModelShowsThreeDashes(t *testing.T) {
	body := ansi.Strip(strings.Join(
		modelDetail(&rafikiv1.ModelRow{Id: "ollama/llama3"}, time.Now(), 160), " "))
	for _, want := range []string{"intel —", "code —", "agentic —"} {
		assert.NewCollecting(t).StrContains(body, want, "%q missing; an unscored model must not read as 0.0:\n", want)
	}
}

func TestScoreCellDistinguishesZeroFromAbsent(t *testing.T) {
	c := assert.NewCollecting(t)
	zero := 0.0
	c.Eq("0.0", scoreCell(&zero), "scoreCell(0)")
	c.Eq("—", scoreCell(nil), "scoreCell(nil)")
}
