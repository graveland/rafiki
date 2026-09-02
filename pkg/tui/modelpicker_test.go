// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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

func loadedPicker(t *testing.T) (*Cockpit, *modelPicker) {
	t.Helper()
	c := formCockpit(t)
	c.form.focus = fieldModel
	c.handleKey(keyMsg("enter"))
	if c.picker == nil {
		t.Fatal("⏎ on the model row did not open the picker")
	}
	c.applyModelsLoaded(modelsLoadedMsg{kind: c.picker.kind, rows: modelRows()})
	return c, c.picker
}

func TestEnterOnTheModelRowBrowsesRatherThanSubmits(t *testing.T) {
	c := formCockpit(t)
	c.form.focus = fieldModel

	_, cmd := c.handleKey(keyMsg("enter"))

	if c.picker == nil {
		t.Fatal("no picker opened")
	}
	if c.form == nil {
		t.Error("the form was dismissed; the picker stacks on top of it")
	}
	if cmd == nil {
		t.Error("no fetch was issued")
	}
}

// Typed text is a head start, not discarded work.
func TestTypedModelTextSeedsTheFilter(t *testing.T) {
	c := formCockpit(t)
	c.form.focus = fieldModel
	c.form.inputs[fieldModel].SetValue("gpt")
	c.handleKey(keyMsg("enter"))

	if got := c.picker.filter.Value(); got != "gpt" {
		t.Errorf("filter = %q, want the typed text carried in", got)
	}
}

func TestFilterNarrowsTheRows(t *testing.T) {
	_, p := loadedPicker(t)
	if len(p.rows) != 3 {
		t.Fatalf("rows = %d, want all 3 before filtering", len(p.rows))
	}
	p.filter.SetValue("deep")
	p.apply()

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
	_, p := loadedPicker(t)
	p.sort = sortCost
	p.apply()

	if got := p.rows[0].GetId(); got != "deepseek/chat" {
		t.Errorf("first row = %q, want the genuinely cheapest", got)
	}
	if got := p.rows[len(p.rows)-1].GetId(); got != "ollama/llama3" {
		t.Errorf("last row = %q, want the unpriced model sorted last", got)
	}
}

func TestBiggestContextSortPutsUnknownContextLast(t *testing.T) {
	_, p := loadedPicker(t)
	p.sort = sortContext
	p.apply()

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
	_, p := loadedPicker(t)
	p.visionOnly = true
	p.apply()

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
	_, p := loadedPicker(t)
	if !strings.Contains(p.footer(), "1 unknown") {
		t.Errorf("footer = %q, want the unknown-capability count", p.footer())
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
	p.apply()

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
	p.apply()

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
	c.form.focus = fieldModel
	c.handleKey(keyMsg("enter"))
	c.applyModelsLoaded(modelsLoadedMsg{kind: c.picker.kind,
		err: errors.New("unavailable: model lister not yet wired")})

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

func TestTabCyclesTheSortAndWraps(t *testing.T) {
	c, p := loadedPicker(t)
	if p.sort != sortID {
		t.Fatalf("initial sort = %v, want sortID", p.sort)
	}
	seen := map[modelSort]bool{p.sort: true}
	for i := 0; i < int(modelSortCount)-1; i++ {
		c.handleKey(keyMsg("tab"))
		if seen[p.sort] {
			t.Fatalf("⇥ revisited %v before covering every sort", p.sort)
		}
		seen[p.sort] = true
	}
	c.handleKey(keyMsg("tab"))
	if p.sort != sortID {
		t.Errorf("sort = %v after a full cycle, want it to wrap to sortID", p.sort)
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
