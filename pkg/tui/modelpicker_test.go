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
	if c.picker == nil {
		t.Fatal("^F on the model row did not open the picker")
	}
	return c, c.picker
}

func TestCtrlFOpensTheFullBrowser(t *testing.T) {
	c := formCockpit(t)
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	if c.picker == nil {
		t.Fatal("no picker opened")
	}
	if c.form == nil {
		t.Error("the form was dismissed; the picker stacks on top of it")
	}
}

// A cached catalog means the browser opens with rows already in it -- no
// second round trip for what the typeahead already fetched.
func TestPickerOpensFromTheCacheWithoutRefetching(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	if c.picker.loading {
		t.Error("picker opened in a loading state despite a warm cache")
	}
	if len(c.picker.rows) != 3 {
		t.Errorf("rows = %d, want the cached 3", len(c.picker.rows))
	}
	// The guard is on fetchModelsCmd itself, so callers can issue it on every
	// event that might need models without tracking state.
	if cmd := c.fetchModelsCmd(c.form.kind()); cmd != nil {
		t.Error("a refetch was issued for a kind already in the cache")
	}
}

// An in-flight fetch must not be started twice: the form opening and the model
// row being reached are two events for the same catalog.
func TestFetchIsNotIssuedTwiceForOneKind(t *testing.T) {
	// A bare cockpit, not formCockpit: opening the form already prefetches,
	// which is itself the guard working.
	c := newTestCockpit("")
	if first := c.fetchModelsCmd("fundi"); first == nil {
		t.Fatal("no fetch issued for a cold cache")
	}
	if second := c.fetchModelsCmd("fundi"); second != nil {
		t.Error("a second fetch was issued while the first was in flight")
	}
}

// Typed text is a head start, not discarded work.
func TestTypedModelTextSeedsTheFilter(t *testing.T) {
	c := formCockpit(t)
	focusModelRow(c)
	c.form.inputs[fieldModel].SetValue("gpt")
	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	if got := c.picker.filter.Value(); got != "gpt" {
		t.Errorf("filter = %q, want the typed text carried in", got)
	}
}

func TestFilterNarrowsTheRows(t *testing.T) {
	c, p := loadedPicker(t)
	if len(p.rows) != 3 {
		t.Fatalf("rows = %d, want all 3 before filtering", len(p.rows))
	}
	p.filter.SetValue("deep")
	p.apply(c.modelView)

	if len(p.rows) != 1 || p.rows[0].GetId() != "deepseek/chat" {
		t.Errorf("rows = %v, want just deepseek/chat", p.rows)
	}
}

// Changing the filter must reset the cursor: leaving it where it was selects
// whatever happens to land under it.
func TestFilterResetsTheCursor(t *testing.T) {
	c, p := loadedPicker(t)
	p.move(+2, 10)
	if p.cursor == 0 {
		t.Fatal("cursor did not move")
	}
	c.handleKey(tea.KeyPressMsg{Code: 'o', Text: "o"})

	if p.cursor != 0 {
		t.Errorf("cursor = %d after typing in the filter, want 0", p.cursor)
	}
}

// ── the presence rules ───────────────────────────────────────────────────────

// An unpriced model is not the cheapest thing available. Sorting an absent
// price as zero is exactly what the optional wire fields exist to prevent.
func TestCheapestSortPutsUnpricedModelsLast(t *testing.T) {
	c, p := loadedPicker(t)
	c.modelView.sort = sortCost
	p.apply(c.modelView)

	if got := p.rows[0].GetId(); got != "deepseek/chat" {
		t.Errorf("first row = %q, want the genuinely cheapest", got)
	}
	if got := p.rows[len(p.rows)-1].GetId(); got != "ollama/llama3" {
		t.Errorf("last row = %q, want the unpriced model sorted last", got)
	}
}

func TestBiggestContextSortPutsUnknownContextLast(t *testing.T) {
	c, p := loadedPicker(t)
	c.modelView.sort = sortContext
	p.apply(c.modelView)

	if got := p.rows[0].GetId(); got != "openai/gpt-4o" {
		t.Errorf("first row = %q, want the biggest context", got)
	}
	if got := p.rows[len(p.rows)-1].GetId(); got != "ollama/llama3" {
		t.Errorf("last row = %q, want the unknown context sorted last", got)
	}
}

// The trap the whole design warns about: empty modalities means the daemon has
// NO catalog entry, not "no vision". A filter that dropped them would hide
// every locally-served model.
func TestVisionFilterKeepsUnknownsAndDropsOnlyKnownTextOnly(t *testing.T) {
	c, p := loadedPicker(t)
	c.modelView.visionOnly = true
	p.apply(c.modelView)

	ids := map[string]bool{}
	for _, r := range p.rows {
		ids[r.GetId()] = true
	}
	if !ids["openai/gpt-4o"] {
		t.Error("vision filter dropped a model that HAS vision")
	}
	if ids["deepseek/chat"] {
		t.Error("vision filter kept a model known to be text-only")
	}
	if !ids["ollama/llama3"] {
		t.Fatal("vision filter dropped an UNKNOWN model; that hides the whole local fleet")
	}
}

// Keeping unknowns is only honest if the user is told. The count is the thing
// that says the ◉ column is not the whole answer.
func TestFooterCountsUnknownCapability(t *testing.T) {
	c, p := loadedPicker(t)
	if !strings.Contains(p.footer(c.modelView), "1 unknown") {
		t.Errorf("footer = %q, want the unknown-capability count", p.footer(c.modelView))
	}
}

func TestAbsentFactsRenderAsDashesNotZeros(t *testing.T) {
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	if got := ctxCell(bare); got != "—" {
		t.Errorf("ctxCell = %q, want an em dash", got)
	}
	if got := priceCell(bare.PromptUsd); got != "—" {
		t.Errorf("priceCell = %q, want an em dash", got)
	}
	if got := visionCellGlyph(bare); got != "?" {
		t.Errorf("visionCellGlyph = %q, want ?", got)
	}
}

// A model priced at zero is genuinely free and must not render as unknown.
func TestZeroPriceRendersAsZeroNotUnknown(t *testing.T) {
	if got := priceCell(fp(0)); got != "0.00" {
		t.Errorf("priceCell(0) = %q, want 0.00", got)
	}
}

// ── selection ────────────────────────────────────────────────────────────────

func TestPickingFillsTheFieldAndReturnsToTheForm(t *testing.T) {
	c, p := loadedPicker(t)
	p.filter.SetValue("gpt")
	p.apply(c.modelView)

	c.handleKey(keyMsg("enter"))

	if c.picker != nil {
		t.Error("picker stayed open after a pick")
	}
	if got := c.form.inputs[fieldModel].Value(); got != "openai/gpt-4o" {
		t.Errorf("model field = %q, want the picked id", got)
	}
	// Focus advances so the next ⏎ submits rather than reopening the picker.
	if c.form.focus == fieldModel {
		t.Error("focus stayed on the model row; ⏎ would reopen the picker")
	}
}

// esc returns to the FORM, not out of both: the other fields are half filled.
func TestEscapeReturnsToTheForm(t *testing.T) {
	c, _ := loadedPicker(t)
	c.handleKey(keyMsg("esc"))

	if c.picker != nil {
		t.Error("esc did not close the picker")
	}
	if c.form == nil {
		t.Fatal("esc dismissed the form too; the half-filled fields are gone")
	}
}

func TestPickingNothingLeavesTheFieldAlone(t *testing.T) {
	c, p := loadedPicker(t)
	c.form.inputs[fieldModel].SetValue("typed/by-hand")
	p.filter.SetValue("no-such-model")
	p.apply(c.modelView)

	c.handleKey(keyMsg("enter"))

	if got := c.form.inputs[fieldModel].Value(); got != "typed/by-hand" {
		t.Errorf("model field = %q, want the hand-typed value untouched", got)
	}
}

// ── failure ──────────────────────────────────────────────────────────────────

// A daemon that cannot answer must not trap the user: the field still accepts
// a hand-typed id.
func TestFetchFailureIsReportedAndRecoverable(t *testing.T) {
	c := formCockpit(t)
	c.applyModelsLoaded(modelsLoadedMsg{kind: c.form.kind(),
		err: errors.New("unavailable: model lister not yet wired")})
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})

	c.width, c.height, c.ready = 100, 30, true
	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "not yet wired") {
		t.Errorf("view did not report the failure:\n%s", out)
	}
	if !strings.Contains(out, "by hand") {
		t.Error("view did not say the id can still be typed by hand")
	}

	c.handleKey(keyMsg("esc"))
	if c.form == nil {
		t.Fatal("esc after a failure dismissed the form")
	}
}

// A late answer for a kind the form no longer has must be dropped, not shown.
func TestStaleFetchIsIgnored(t *testing.T) {
	c, _ := loadedPicker(t)
	c.applyModelsLoaded(modelsLoadedMsg{kind: "some-other-kind",
		rows: []*rafikiv1.ModelRow{{Id: "wrong/model"}}})

	for _, r := range c.picker.rows {
		if r.GetId() == "wrong/model" {
			t.Fatal("a stale fetch for another kind was applied")
		}
	}
}

func TestPickerOwnsTheBodyPaneAboveTheForm(t *testing.T) {
	c, _ := loadedPicker(t)
	c.width, c.height, c.ready = 100, 30, true

	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "openai/gpt-4o") {
		t.Error("the picker did not render")
	}
	if strings.Contains(out, "new agent") {
		t.Error("the form rendered over the picker")
	}
}

func TestCtrlSCyclesTheSortAndWraps(t *testing.T) {
	c, _ := loadedPicker(t)
	if c.modelView.sort != sortID {
		t.Fatalf("initial sort = %v, want sortID", c.modelView.sort)
	}
	seen := map[modelSort]bool{c.modelView.sort: true}
	for i := 0; i < int(modelSortCount)-1; i++ {
		c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		if seen[c.modelView.sort] {
			t.Fatalf("^S revisited %v before covering every sort", c.modelView.sort)
		}
		seen[c.modelView.sort] = true
	}
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if c.modelView.sort != sortID {
		t.Errorf("sort = %v after a full cycle, want it to wrap", c.modelView.sort)
	}
}

// Every sort must have a label. An unnamed one renders as "?" in the footer,
// which is the only place the active sort is visible at all.
func TestEverySortIsNamed(t *testing.T) {
	for s := modelSort(0); s < modelSortCount; s++ {
		if s.String() == "?" {
			t.Errorf("sort %d has no label", s)
		}
	}
}

// ── live typeahead ───────────────────────────────────────────────────────────

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

	if !c.modelsBusy["fundi"] {
		t.Error("no catalog fetch was started when the form opened")
	}
}

// An empty model field still lists, so ↓ browses. An empty box that answers
// nothing looks broken.
func TestEmptyModelFieldStillSuggests(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	if !c.form.showSuggestions() {
		t.Error("no suggestions for an empty field; ↓ would have nothing to browse")
	}
}

// The list follows FOCUS: tabbing away must not leave it floating under a
// field nobody is editing.
func TestSuggestionsHideWhenTheModelRowLosesFocus(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	if !c.form.showSuggestions() {
		t.Fatal("no suggestions to begin with")
	}

	c.handleKey(keyMsg("tab"))

	if c.form.showSuggestions() {
		t.Error("suggestions still showing after the model row lost focus")
	}
}

// ↓ on the model row walks INTO the list rather than to the next field.
func TestDownEntersTheSuggestionList(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(keyMsg("down"))

	if c.form.focus != fieldModel {
		t.Fatal("↓ left the model row instead of entering the list")
	}
	if c.form.suggestCur != 0 {
		t.Errorf("suggestCur = %d, want 0", c.form.suggestCur)
	}
}

// ↑ off the top of the list returns to the text, not to the previous field.
func TestUpOffTheListReturnsToTheText(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))

	c.handleKey(keyMsg("up"))

	if c.form.suggestCur != -1 {
		t.Errorf("suggestCur = %d, want -1", c.form.suggestCur)
	}
	if c.form.focus != fieldModel {
		t.Error("↑ left the model row; the way out of a typeahead is back to the text")
	}
}

// ↑ from the SECOND row lands on the first, not back in the text. Clamping
// the decrement at 0 and then treating 0 as "leave the list" skips the top row
// entirely, which makes the first suggestion unreachable with the keyboard.
func TestUpFromTheSecondRowLandsOnTheFirst(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	c.handleKey(keyMsg("down"))
	if c.form.suggestCur != 1 {
		t.Fatalf("suggestCur = %d, want 1 before the ↑", c.form.suggestCur)
	}

	c.handleKey(keyMsg("up"))

	if c.form.suggestCur != 0 {
		t.Errorf("suggestCur = %d, want 0 — the top row must be reachable", c.form.suggestCur)
	}
}

// ⏎ takes the highlighted suggestion.
func TestEnterTakesTheHighlightedSuggestion(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))

	_, cmd := c.handleKey(keyMsg("enter"))

	if got := c.form.inputs[fieldModel].Value(); got == "" {
		t.Fatal("⏎ on a highlighted suggestion filled nothing")
	}
	if cmd != nil {
		t.Error("⏎ on a suggestion also submitted the form")
	}
}

// ...and with NOTHING highlighted it submits, exactly as on every other row.
// This is what stops ⏎ meaning two things at the same moment.
func TestEnterWithNoHighlightSubmits(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	if c.form.suggestCur != -1 {
		t.Fatal("something was highlighted before any ↓")
	}

	_, cmd := c.handleKey(keyMsg("enter"))

	if cmd == nil {
		t.Error("⏎ with no highlight did not submit")
	}
}

// Retyping must drop the highlight: a cursor left on row 3 of the OLD list
// selects whatever now happens to sit there.
func TestTypingClearsTheHighlight(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	if c.form.suggestCur != 0 {
		t.Fatal("nothing highlighted to begin with")
	}

	c.handleKey(tea.KeyPressMsg{Code: 'o', Text: "o"})

	if c.form.suggestCur != -1 {
		t.Errorf("suggestCur = %d after typing, want -1", c.form.suggestCur)
	}
}

// The two kinds have different model universes, so cycling kind must rebuild
// the list from the other catalog rather than leave the old one showing.
func TestCyclingKindRebuildsTheSuggestions(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, protocol.KindFundi, modelRows())
	seedModels(c, protocol.KindClaude, []*rafikiv1.ModelRow{
		{Id: "anthropic/claude-opus-5", ContextWindow: i32p(200000)},
	})
	focusModelRow(c)
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
	if len(c.form.suggest) != 3 {
		t.Fatalf("suggest = %d, want the 3 fundi rows", len(c.form.suggest))
	}

	c.form.focus = fieldKind
	c.handleKey(keyMsg("right"))

	if c.form.kind() != protocol.KindClaude {
		t.Fatalf("kind = %q, want claude", c.form.kind())
	}
	if len(c.form.suggest) != 1 || c.form.suggest[0].GetId() != "anthropic/claude-opus-5" {
		t.Errorf("suggest = %v, want the claude catalog", c.form.suggest)
	}
}

// The list fills the panel rather than a fixed handful, and holds EVERY match
// so a filter hitting 40 models is navigable instead of silently truncated.
func TestSuggestionsFillThePanelAndKeepEveryMatch(t *testing.T) {
	c := formCockpit(t)
	many := make([]*rafikiv1.ModelRow, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, &rafikiv1.ModelRow{Id: fmt.Sprintf("x/model-%02d", i)})
	}
	seedModels(c, c.form.kind(), many)
	focusModelRow(c)

	if len(c.form.suggest) != 40 {
		t.Errorf("suggest = %d, want every match retained", len(c.form.suggest))
	}
	// A tall pane shows more rows than a short one; that is the whole request.
	tall := c.form.suggestWindow(40)
	short := c.form.suggestWindow(14)
	if tall <= short {
		t.Errorf("window: tall=%d short=%d, want the taller pane to show more", tall, short)
	}
	// The view renders the window PLUS the fixed-height detail block.
	if got, want := strings.Count(c.form.suggestView(90, tall, c.modelView), "\n"), tall+detailHeight; got != want {
		t.Errorf("rendered %d rows, want %d (window %d + detail %d)", got, want, tall, detailHeight)
	}
}

// Walking past the bottom of the window scrolls rather than stopping.
func TestSuggestionListScrolls(t *testing.T) {
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
	if c.form.suggestCur != 11 {
		t.Fatalf("suggestCur = %d, want 11", c.form.suggestCur)
	}
	if c.form.suggestOff == 0 {
		t.Error("the window never scrolled; rows past the first screenful are unreachable")
	}
	if c.form.suggestCur < c.form.suggestOff ||
		c.form.suggestCur >= c.form.suggestOff+window {
		t.Errorf("cursor %d outside the drawn window [%d,%d)",
			c.form.suggestCur, c.form.suggestOff, c.form.suggestOff+window)
	}
}

// A new filter restarts the window, not just the highlight: scrolled deep into
// the old list, the new one would otherwise open somewhere arbitrary.
func TestFilteringResetsTheScrollWindow(t *testing.T) {
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
	if c.form.suggestOff == 0 {
		t.Fatal("did not scroll")
	}

	c.handleKey(tea.KeyPressMsg{Code: '3', Text: "3"})

	if c.form.suggestOff != 0 {
		t.Errorf("suggestOff = %d after retyping, want 0", c.form.suggestOff)
	}
}

// Each suggestion carries the facts that decide the choice; the id alone does
// not answer "which of these three opus ids".
func TestSuggestionsShowTheDecidingFacts(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	out := ansi.Strip(c.form.suggestView(90, 10, c.modelView))
	if !strings.Contains(out, "128k") {
		t.Error("no context column in the typeahead")
	}
	if !strings.Contains(out, "5.00") {
		t.Error("no price column in the typeahead")
	}
	if !strings.Contains(out, "?") {
		t.Error("the unknown-capability model does not render as unknown")
	}
}

// ── sort and vision, shared by both views ────────────────────────────────────

// The ask: sorting must work inline, not only after opening the full browser.
func TestCtrlSSortsTheInlineTypeahead(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	if got := c.form.suggest[0].GetId(); got != "deepseek/chat" {
		t.Fatalf("first suggestion = %q, want alphabetical order to start", got)
	}

	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})

	if c.modelView.sort != sortCost {
		t.Fatalf("sort = %v, want sortCost", c.modelView.sort)
	}
	if got := c.form.suggest[0].GetId(); got != "deepseek/chat" {
		t.Errorf("first suggestion = %q, want the cheapest", got)
	}
	if got := c.form.suggest[len(c.form.suggest)-1].GetId(); got != "ollama/llama3" {
		t.Errorf("last suggestion = %q, want the unpriced model last", got)
	}
}

func TestCtrlVFiltersVisionInTheInlineTypeahead(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})

	ids := map[string]bool{}
	for _, r := range c.form.suggest {
		ids[r.GetId()] = true
	}
	if ids["deepseek/chat"] {
		t.Error("a model known to be text-only survived the vision filter")
	}
	if !ids["ollama/llama3"] {
		t.Error("an UNKNOWN-capability model was dropped; that hides the local fleet")
	}
}

// One setting, two windows. Sorting inline and then opening the browser must
// not silently reorder under you.
func TestSortCarriesFromTheTypeaheadIntoTheBrowser(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) // → cheapest

	c.handleKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl}) // open browser

	if c.picker == nil {
		t.Fatal("browser did not open")
	}
	if c.picker.rows[len(c.picker.rows)-1].GetId() != "ollama/llama3" {
		t.Error("the browser opened in a different order than the typeahead")
	}
}

// ...and back the other way.
func TestSortCarriesFromTheBrowserBackToTheTypeahead(t *testing.T) {
	c, _ := loadedPicker(t)
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	want := c.modelView.sort

	c.handleKey(keyMsg("esc")) // back to the form
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)

	if c.modelView.sort != want {
		t.Errorf("sort = %v after returning to the form, want %v", c.modelView.sort, want)
	}
}

// The two views must never disagree about what matches. They had separate
// copies of this logic, which is exactly how they would drift.
func TestBothViewsSelectIdenticallyForTheSameQuery(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.modelView = modelView{sort: sortCost, visionOnly: true}

	c.form.inputs[fieldModel].SetValue("a")
	c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
	inline := c.form.suggest

	p := newModelPicker(c.form.kind(), "a", c.models[c.form.kind()], true, "", c.modelView)

	if len(inline) != len(p.rows) {
		t.Fatalf("typeahead %d rows, browser %d — the two disagree", len(inline), len(p.rows))
	}
	for i := range inline {
		if inline[i].GetId() != p.rows[i].GetId() {
			t.Errorf("row %d: typeahead %q, browser %q", i, inline[i].GetId(), p.rows[i].GetId())
		}
	}
}

// The active sort is otherwise invisible, and an on vision filter looks like a
// catalog that happens to hold no text-only models.
func TestHintLineNamesTheActiveView(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.modelView = modelView{sort: sortCost, visionOnly: true}

	out := ansi.Strip(c.form.view(90, 24, c.modelView))
	if !strings.Contains(out, "cheapest") {
		t.Errorf("hint line does not name the sort:\n%s", out)
	}
	if !strings.Contains(out, "vision on") {
		t.Errorf("hint line does not say the vision filter is on:\n%s", out)
	}
}

// Changing sort or filter must drop the highlight: it names a row in the OLD
// order, and keeping it selects whatever now sits there.
func TestChangingTheViewClearsTheHighlight(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)
	c.handleKey(keyMsg("down"))
	if c.form.suggestCur != 0 {
		t.Fatal("nothing highlighted to begin with")
	}

	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})

	if c.form.suggestCur != -1 {
		t.Errorf("suggestCur = %d after a re-sort, want -1", c.form.suggestCur)
	}
}

// ── tool support, age, expiry ────────────────────────────────────────────────

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
	c := formCockpit(t)
	if !c.modelView.toolsOnly {
		t.Fatal("toolsOnly defaults off; a non-agentic model would be offered")
	}
	seedModels(c, c.form.kind(), toolRows())
	focusModelRow(c)

	ids := map[string]bool{}
	for _, r := range c.form.suggest {
		ids[r.GetId()] = true
	}
	if ids["b/chat-only"] {
		t.Error("a model known not to support tools was offered by default")
	}
	if !ids["a/agentic"] {
		t.Error("a tool-capable model was hidden")
	}
	// The same trap as vision: nil means no catalog entry, which is every
	// locally-served model. Reading it as "no tools" hides the local fleet.
	if !ids["c/unknown"] {
		t.Fatal("an UNKNOWN-capability model was hidden by the default filter")
	}
}

func TestCtrlTRevealsNonToolModels(t *testing.T) {
	c := formCockpit(t)
	seedModels(c, c.form.kind(), toolRows())
	focusModelRow(c)

	c.handleKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})

	if c.modelView.toolsOnly {
		t.Fatal("^T did not toggle the filter")
	}
	found := false
	for _, r := range c.form.suggest {
		if r.GetId() == "b/chat-only" {
			found = true
		}
	}
	if !found {
		t.Error("^T did not reveal the non-tool model")
	}
}

// Off is the notable state, because on is the default: a list silently
// including models that cannot be agents is the surprising one.
func TestHintLineFlagsWhenNonToolModelsAreIncluded(t *testing.T) {
	v := defaultModelView()
	if strings.Contains(v.summary(), "no-tools") {
		t.Error("the default view advertises a filter that is simply on")
	}
	v.toggleTools()
	if !strings.Contains(v.summary(), "no-tools") {
		t.Errorf("summary = %q, want it to flag that non-tool models are included", v.summary())
	}
}

func TestNewestSortOrdersByListingDateAndPutsUnknownLast(t *testing.T) {
	c, p := loadedPicker(t)
	seedModels(c, c.picker.kind, toolRows())
	c.modelView = modelView{sort: sortNewest}
	p.all = toolRows()
	p.apply(c.modelView)

	if got := p.rows[0].GetId(); got != "a/agentic" {
		t.Errorf("first row = %q, want the newest", got)
	}
	if got := p.rows[len(p.rows)-1].GetId(); got != "c/unknown" {
		t.Errorf("last row = %q, want the undated model last", got)
	}
}

// Sorting by something invisible is a list that reorders for no reason, so the
// AGE column appears exactly when it is the sort key.
func TestAgeColumnAppearsOnlyWhenSortingByAge(t *testing.T) {
	if _, _, ok := sortID.column(); ok {
		t.Error("sortID asked for an extra column; it sorts by a pinned one")
	}
	if _, _, ok := sortCost.column(); ok {
		t.Error("sortCost asked for an extra column; price is already pinned")
	}
	title, w, ok := sortNewest.column()
	if !ok || title != "AGE" || w <= 0 {
		t.Errorf("sortNewest.column() = (%q,%d,%v), want an AGE column", title, w, ok)
	}
}

func TestAgeCellIsCoarseAndAbsenceIsADash(t *testing.T) {
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
		if got := ageCell(mk(tc.off), now); got != tc.want {
			t.Errorf("ageCell(%dd) = %q, want %q", tc.off/day, got, tc.want)
		}
	}
	if got := ageCell(&rafikiv1.ModelRow{}, now); got != "—" {
		t.Errorf("ageCell(absent) = %q, want an em dash", got)
	}
}

// Expiry is a forward warning, and the far-future sentinel is not one.
func TestExpiryWarnsOnlyWithinAYear(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	soon := &rafikiv1.ModelRow{ExpiresAt: "2026-09-08"}
	if got := expiryWarning(soon, now); !strings.Contains(got, "6d") ||
		!strings.Contains(got, "2026-09-08") {
		t.Errorf("expiryWarning(soon) = %q, want the date and the countdown", got)
	}
	// "2098-12-31" means "no planned removal"; warning on it would put a
	// notice next to models in no danger at all.
	sentinel := &rafikiv1.ModelRow{ExpiresAt: "2098-12-31"}
	if got := expiryWarning(sentinel, now); got != "" {
		t.Errorf("expiryWarning(sentinel) = %q, want silence", got)
	}
	if got := expiryWarning(&rafikiv1.ModelRow{}, now); got != "" {
		t.Errorf("expiryWarning(none) = %q, want empty", got)
	}
	if got := expiryWarning(&rafikiv1.ModelRow{ExpiresAt: "not-a-date"}, now); got != "" {
		t.Errorf("expiryWarning(garbage) = %q, want empty", got)
	}
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
	if !strings.Contains(out, "thinking yes") {
		t.Errorf("detail does not report reasoning support:\n%s", out)
	}
}

// Fixed position is the whole point: every label is present whether or not it
// has a value, so the eye returns to the same column for the same fact.
func TestDetailBlockLabelsEveryFieldEvenWhenAbsent(t *testing.T) {
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	lines := modelDetail(bare, time.Now(), 140)
	if len(lines) != detailHeight {
		t.Fatalf("detail is %d lines, want a fixed %d", len(lines), detailHeight)
	}
	body := ansi.Strip(lines[1] + " " + lines[2])
	for _, label := range []string{"source", "age", "ctx", "max out", "in/out",
		"cache", "tools", "vision", "thinking"} {
		if !strings.Contains(body, label) {
			t.Errorf("label %q missing for a model with no facts:\n%s", label, body)
		}
	}
	if strings.Count(body, "—") < 4 {
		t.Errorf("absent values should read as em dashes:\n%s", body)
	}
}

// The block keeps its height with nothing highlighted, so the list above it
// does not grow and shrink as the cursor moves.
func TestDetailBlockKeepsItsHeightWhenEmpty(t *testing.T) {
	if got := len(modelDetail(nil, time.Now(), 80)); got != detailHeight {
		t.Errorf("empty detail is %d lines, want %d", got, detailHeight)
	}
}

// A rule separates the block from the list; without it the two read as one.
func TestDetailBlockIsSeparatedFromTheList(t *testing.T) {
	lines := modelDetail(&rafikiv1.ModelRow{Id: "a/b"}, time.Now(), 40)
	if !strings.Contains(ansi.Strip(lines[0]), "───") {
		t.Errorf("no rule above the detail block: %q", ansi.Strip(lines[0]))
	}
}

// "unknown" is a real answer and must never render as "no": the daemon has no
// catalog entry for any locally-served model.
func TestDetailBlockSpellsUnknownRatherThanNo(t *testing.T) {
	bare := &rafikiv1.ModelRow{Id: "ollama/llama3"}
	body := ansi.Strip(strings.Join(modelDetail(bare, time.Now(), 140), " "))
	if !strings.Contains(body, "tools unknown") {
		t.Errorf("tools rendered as something other than unknown:\n%s", body)
	}
	if !strings.Contains(body, "vision unknown") {
		t.Errorf("vision rendered as something other than unknown:\n%s", body)
	}
}

// A no-tools model reachable only via ^T must be labelled where it is picked.
func TestDetailBlockFlagsANoToolsModel(t *testing.T) {
	row := &rafikiv1.ModelRow{Id: "b/chat-only", SupportedParameters: []string{"temperature"}}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	if !strings.Contains(body, "tools NO") {
		t.Errorf("a model that cannot tool-call is not flagged:\n%s", body)
	}
}

// The expiry warning rides the block rather than a column of its own.
func TestDetailBlockCarriesTheExpiryWarning(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	row := &rafikiv1.ModelRow{Id: "a/b", ExpiresAt: "2026-09-08"}
	body := ansi.Strip(strings.Join(modelDetail(row, now, 140), " "))
	if !strings.Contains(body, "removed 2026-09-08 (6d)") {
		t.Errorf("no expiry warning in the detail block:\n%s", body)
	}
}

// Every value must FIT its cell. A width that clips "unknown" to "unkno…" is
// worse than the free-form line this block replaced, and only a rendered
// check catches it -- the fields are all present either way.
func TestDetailBlockCellsAreWideEnoughForTheirValues(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	f := func(v float64) *float64 { return &v }
	worst := &rafikiv1.ModelRow{
		Id: "x/y", Source: "openrouter",
		ContextWindow: i32(1000000), MaxCompletionTokens: i32(128000),
		PromptUsd: f(0.00001), CompletionUsd: f(0.0001),
		CacheReadUsd: f(0.000001), CacheWriteUsd: f(0.0000125),
		KnowledgeCutoff: "2026-02-16", AgenticIndex: f(100.0),
		// no supported_parameters and no modalities: both read "unknown",
		// which are the longest values these cells ever hold.
	}
	body := ansi.Strip(strings.Join(modelDetail(worst, time.Now(), 130), " "))
	if strings.Contains(body, "…") {
		t.Errorf("a detail cell clipped its own value:\n%s", body)
	}
	for _, want := range []string{"tools unknown", "vision unknown",
		"source openrouter", "ctx 1.0M", "max out 128k", "in/out 10.00/100.00",
		"cutoff 2026-02-16", "agentic 100.0", "thinking no"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing or clipped:\n%s", want, body)
		}
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
	c, p := loadedPicker(t)
	c.modelView = modelView{sort: sortAgentic}
	p.all = scoredRows()
	p.apply(c.modelView)

	if got := p.rows[0].GetId(); got != "b/best" {
		t.Errorf("first row = %q, want the highest score", got)
	}
	// Absent is UNSCORED, not zero. Sorting it below a 0.3 would be the same
	// failure absent pricing would be under "cheapest".
	if got := p.rows[len(p.rows)-1].GetId(); got != "c/unscored" {
		t.Errorf("last row = %q, want the unscored model last", got)
	}
}

// Sorting by a score you cannot see is a list that reorders for no reason.
func TestAgenticSortShowsItsOwnColumn(t *testing.T) {
	title, w, ok := sortAgentic.column()
	if !ok || title != "AGENTIC" || w <= 0 {
		t.Fatalf("sortAgentic.column() = (%q,%d,%v), want an AGENTIC column", title, w, ok)
	}
	f := 59.2
	row := &rafikiv1.ModelRow{Id: "b/best", AgenticIndex: &f}
	if got := extraCell(row, sortAgentic, time.Now()); got != "59.2" {
		t.Errorf("extraCell = %q, want the score", got)
	}
	if got := extraCell(row, sortNewest, time.Now()); got != "—" {
		t.Errorf("extraCell under sortNewest = %q, want the age cell", got)
	}
}

func TestUnscoredAndUncutModelsReadAsAbsentNotZero(t *testing.T) {
	bare := &rafikiv1.ModelRow{Id: "c/unscored"}
	if got := agenticCell(bare); got != "—" {
		t.Errorf("agenticCell = %q, want an em dash, never 0.0", got)
	}
	if got := cutoffCell(bare); got != "—" {
		t.Errorf("cutoffCell = %q, want an em dash", got)
	}
	// A genuinely low score is a real value and must not read as absent.
	low := 0.3
	if got := agenticCell(&rafikiv1.ModelRow{AgenticIndex: &low}); got != "0.3" {
		t.Errorf("agenticCell(0.3) = %q, want 0.3", got)
	}
}

func TestDetailBlockCarriesCutoffAndAgenticScore(t *testing.T) {
	f := 59.2
	row := &rafikiv1.ModelRow{Id: "b/best", AgenticIndex: &f, KnowledgeCutoff: "2026-02-16"}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	if !strings.Contains(body, "agentic 59.2") {
		t.Errorf("no agentic score in the detail block:\n%s", body)
	}
	if !strings.Contains(body, "cutoff 2026-02-16") {
		t.Errorf("no knowledge cutoff in the detail block:\n%s", body)
	}
}

// Cutoff and age are different axes and both earn a slot: a model listed last
// week can have a cutoff from a year before that.
func TestCutoffAndAgeAreSeparateFields(t *testing.T) {
	created := time.Now().AddDate(0, 0, -7).Unix()
	row := &rafikiv1.ModelRow{Id: "x/y", Created: &created, KnowledgeCutoff: "2025-01-31"}
	body := ansi.Strip(strings.Join(modelDetail(row, time.Now(), 140), " "))
	if !strings.Contains(body, "age 7d") {
		t.Errorf("age missing or wrong:\n%s", body)
	}
	if !strings.Contains(body, "cutoff 2025-01-31") {
		t.Errorf("cutoff missing:\n%s", body)
	}
}
