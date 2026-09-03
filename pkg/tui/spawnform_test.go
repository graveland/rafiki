// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+up":
		return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
	case "shift+down":
		return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func formCockpit(t *testing.T) *Cockpit {
	t.Helper()
	c := railWith(t, "c_1")
	c.handleKey(keyMsg("n"))
	if c.form == nil {
		t.Fatal("n on the agents pane did not open the create form")
	}
	return c
}

func TestNOpensTheCreateForm(t *testing.T) {
	formCockpit(t)
}

// n must do nothing from the input pane: it is a letter, and typing "now" into
// a prompt must not spawn an agent.
func TestNInTheInputPaneIsJustALetter(t *testing.T) {
	c := newTestCockpit("c_1")
	c.focus = focusInput
	c.handleKey(keyMsg("n"))

	if c.form != nil {
		t.Fatal("n opened the create form while typing")
	}
	if !strings.Contains(c.ta.Value(), "n") {
		t.Errorf("textarea = %q, want the letter to have been typed", c.ta.Value())
	}
}

// The modal is checked BEFORE the globals, so tab must move between fields
// rather than reaching cyclePane.
func TestTabCyclesFieldsRatherThanPanes(t *testing.T) {
	c := formCockpit(t)
	before := c.focus

	c.handleKey(keyMsg("tab"))

	if c.form.focus != fieldKind {
		t.Errorf("form focus = %v, want fieldKind", c.form.focus)
	}
	if c.focus != before {
		t.Error("tab reached the pane ring; a modal must own that key")
	}
}

func TestFieldFocusWrapsBothWays(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("shift+tab"))
	if c.form.focus != spawnFieldCount-1 {
		t.Errorf("focus = %v, want the last field after wrapping back", c.form.focus)
	}
	c.handleKey(keyMsg("tab"))
	if c.form.focus != fieldName {
		t.Errorf("focus = %v, want fieldName after wrapping forward", c.form.focus)
	}
}

func TestKindCyclesAndIsNeverFreeText(t *testing.T) {
	c := formCockpit(t)
	c.form.focus = fieldKind

	first := c.form.kind()
	c.handleKey(keyMsg("right"))
	if c.form.kind() == first {
		t.Fatal("→ did not change the kind")
	}
	c.handleKey(keyMsg("left"))
	if c.form.kind() != first {
		t.Error("← did not restore the kind; the cycle must be symmetric")
	}

	// A letter on the kind row must be swallowed, not typed anywhere.
	c.handleKey(keyMsg("z"))
	for _, k := range spawnKinds {
		if c.form.kind() == k {
			return
		}
	}
	t.Errorf("kind = %q, which is not one of %v", c.form.kind(), spawnKinds)
}

// space cycles the kind row too. bubbletea spells the key "space", so a
// `case " "` matches nothing and the binding is dead with no error anywhere --
// which is exactly how it shipped the first time.
func TestSpaceCyclesTheKindRow(t *testing.T) {
	c := formCockpit(t)
	c.form.focus = fieldKind
	first := c.form.kind()

	c.handleKey(keyMsg("space"))

	if c.form.kind() == first {
		t.Errorf("space did not cycle the kind; still %q", first)
	}
}

func TestEscapeCancelsTheForm(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("esc"))
	if c.form != nil {
		t.Error("esc did not close the form")
	}
}

// ^C in a modal cancels the modal. It must NOT arm the cockpit's quit.
func TestCtrlCCancelsTheFormWithoutArmingQuit(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	if c.form != nil {
		t.Error("^C did not close the form")
	}
	if !c.quitArmed.IsZero() {
		t.Error("^C armed quit from inside a modal")
	}
}

// cwd is required by the server. Catching it here keeps the values on screen
// instead of spending a round trip to be told.
func TestEmptyCwdIsRefusedLocally(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldCwd].SetValue("")

	_, cmd := c.handleKey(keyMsg("enter"))

	if cmd != nil {
		t.Error("submitted with no cwd; the server would refuse it")
	}
	if c.form == nil {
		t.Fatal("form closed on a validation failure")
	}
	if !strings.Contains(c.form.err, "cwd") {
		t.Errorf("err = %q, want it to name cwd", c.form.err)
	}
}

func TestCwdIsPrefilled(t *testing.T) {
	c := formCockpit(t)
	if c.form.inputs[fieldCwd].Value() == "" {
		t.Error("cwd was not prefilled; every create would need it typed by hand")
	}
}

// A refused spawn keeps the form and its values: the daemon's complaint is
// usually about one field, and dismissing throws away what needs correcting.
func TestSpawnFailureKeepsTheFormAndItsValues(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldName].SetValue("scout")
	c.form.busy = true

	c.applySpawned(spawnedMsg{err: errors.New("internal: no such directory")})

	if c.form == nil {
		t.Fatal("a failed spawn dismissed the form")
	}
	if c.form.inputs[fieldName].Value() != "scout" {
		t.Error("the typed name was lost")
	}
	if c.form.busy {
		t.Error("form still busy after a failure; a retry would be impossible")
	}
	if !strings.Contains(c.form.err, "no such directory") {
		t.Errorf("err = %q, want the daemon's reason", c.form.err)
	}
}

func TestSpawnSuccessClosesTheForm(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	c.applySpawned(spawnedMsg{childID: "c_new"})

	if c.form != nil {
		t.Error("form stayed open after a successful create")
	}
}

// A second Enter while the first is in flight must not create two agents.
func TestBusyFormRefusesASecondSubmit(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	if _, cmd := c.handleKey(keyMsg("enter")); cmd != nil {
		t.Error("a busy form submitted again")
	}
}

// The modal must render instead of the transcript, and ahead of the help sheet.
func TestFormOwnsTheBodyPane(t *testing.T) {
	c := formCockpit(t)
	c.showHelp = true
	c.width, c.height, c.ready = 100, 30, true

	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "new agent") {
		t.Error("the form did not render")
	}
	if strings.Contains(out, "closes this") {
		t.Error("the help sheet rendered over the modal")
	}
}

func TestFormShowsEveryFieldAndBothKinds(t *testing.T) {
	c := formCockpit(t)
	out := c.form.view(80, 24, c.modelView, nil, nil)
	for _, want := range []string{"name", "kind", "model", "cwd"} {
		if !strings.Contains(out, want) {
			t.Errorf("form view is missing the %q row", want)
		}
	}
	// Both kinds are shown, not just the selected one: a single value gives no
	// hint that the row can change.
	for _, k := range spawnKinds {
		if !strings.Contains(out, k) {
			t.Errorf("form view does not offer kind %q", k)
		}
	}
}

// A bubbles input is constructed BLURRED and its Update returns immediately
// while it is, swallowing every printable key with no error anywhere. The
// cockpit shipped exactly that bug once — the whole three-pane UI could not be
// typed into — so the form gets the same guard the textarea has: drive a rune
// through the real key path and assert it landed.
func TestTypingReachesTheFocusedFormField(t *testing.T) {
	c := formCockpit(t)

	for _, r := range "scout" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if got := c.form.inputs[fieldName].Value(); got != "scout" {
		t.Fatalf("name field = %q, want scout — the input is not focused", got)
	}
}

// Focus must MOVE with the tab order, not stay on the first field. Blurring the
// old row and focusing the new one are two separate calls, and getting only the
// first right yields a form where every row after the first is dead.
func TestTypingFollowsTheFocusedField(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("tab")) // kind
	c.handleKey(keyMsg("tab")) // model

	for _, r := range "opus" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if got := c.form.inputs[fieldModel].Value(); got != "opus" {
		t.Errorf("model field = %q, want opus", got)
	}
	if got := c.form.inputs[fieldName].Value(); got != "" {
		t.Errorf("name field = %q, want empty — the old row kept focus", got)
	}
}

// A modal takes the WHOLE panel: a rail behind the create form is a list you
// cannot act on, costing width from a table that needs it.
func TestModalsHideTheRail(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	c.width, c.height, c.ready = 100, 30, true
	if c.railCols() == 0 {
		t.Fatal("no rail to begin with")
	}
	before := c.convWidth()

	c.handleKey(keyMsg("n"))

	if c.railCols() != 0 {
		t.Error("the rail is still drawn behind the create form")
	}
	if c.convWidth() <= before {
		t.Error("the form did not get the width the rail gave up")
	}
	if strings.Contains(ansi.Strip(c.View().Content), "c_2") {
		t.Error("a rail row rendered behind the modal")
	}

	c.handleKey(keyMsg("esc"))
	if c.railCols() == 0 {
		t.Error("the rail did not come back when the modal closed")
	}
}

// `rafiki create` with nothing to go on opens straight into the form, prefilled
// with what a bare create would have spawned — so the default case costs one ⏎
// and shows what it is about to do.
func TestOpenCreatePrefillsTheForm(t *testing.T) {
	c := NewCockpit(Options{
		BaseURL:    "http://127.0.0.1:1",
		OpenCreate: true,
		CreateDefaults: SpawnDefaults{
			Name: "reviewer", Kind: "claude", Model: "anthropic/claude-opus-5", Cwd: "/tmp/x",
		},
	})
	if c.form == nil {
		t.Fatal("OpenCreate did not open the form")
	}
	if got := c.form.inputs[fieldName].Value(); got != "reviewer" {
		t.Errorf("name = %q", got)
	}
	if got := c.form.kind(); got != "claude" {
		t.Errorf("kind = %q, want claude", got)
	}
	if got := c.form.inputs[fieldModel].Value(); got != "anthropic/claude-opus-5" {
		t.Errorf("model = %q", got)
	}
	if got := c.form.inputs[fieldCwd].Value(); got != "/tmp/x" {
		t.Errorf("cwd = %q", got)
	}
}

// ExecutorSelector is not a form field (spawnForm deliberately stays five
// fields), so the only way to check it survived construction is the private
// field it lands on.
func TestOpenCreateCarriesTheExecutorSelector(t *testing.T) {
	c := NewCockpit(Options{
		BaseURL:          "http://127.0.0.1:1",
		OpenCreate:       true,
		ExecutorSelector: "owner=brent",
	})
	if c.executorSelector != "owner=brent" {
		t.Errorf("executorSelector = %q, want owner=brent", c.executorSelector)
	}
}

// Empty defaults keep the form's own, rather than blanking the prefilled cwd.
func TestOpenCreateWithNoDefaultsKeepsTheFormsOwn(t *testing.T) {
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", OpenCreate: true})
	if c.form == nil {
		t.Fatal("OpenCreate did not open the form")
	}
	if c.form.inputs[fieldCwd].Value() == "" {
		t.Error("cwd prefill was cleared by an empty default")
	}
}

// A form opened at CONSTRUCTION never saw the `n` keypress that normally starts
// the catalog fetch, so Init has to start it or the typeahead sits empty.
func TestOpenCreateFetchesTheCatalog(t *testing.T) {
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", OpenCreate: true})
	_ = c.Init()
	if !c.modelsBusy[c.form.kind()] {
		t.Error("no catalog fetch was started for a form opened at construction")
	}
}

// bubbles renders a ONE-character placeholder when Width is unset:
// placeholderView sizes its buffer to Width()+1, copies the placeholder in,
// and early-returns having emitted only p[:1]. "(auto)" came out as "(" and
// the picker's "filter…" as "f", in shipped output nobody read closely.
func TestPlaceholdersRenderInFull(t *testing.T) {
	c := formCockpit(t)
	out := ansi.Strip(c.form.view(90, 24, c.modelView, nil, nil))
	for _, want := range []string{"(auto)", "(daemon default)"} {
		if !strings.Contains(out, want) {
			t.Errorf("placeholder %q is missing or truncated:\n%s", want, out)
		}
	}
}

func TestPickerFilterPlaceholderRendersInFull(t *testing.T) {
	c, p := loadedPicker(t)
	p.filter.SetValue("")
	out := ansi.Strip(p.view(90, 20, c.modelView, nil))
	if !strings.Contains(out, "filter…") {
		t.Errorf("the filter placeholder is truncated:\n%s", out)
	}
}

// A field the user has typed into must show what they typed, not a placeholder
// and not a truncation of it.
func TestTypedValueSurvivesTheWidthChange(t *testing.T) {
	c := formCockpit(t)
	focusModelRow(c)
	for _, r := range "anthropic/claude-opus-5" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	out := ansi.Strip(c.form.view(120, 24, c.modelView, nil, nil))
	if !strings.Contains(out, "anthropic/claude-opus-5") {
		t.Errorf("the typed model id is not shown in full:\n%s", out)
	}
}

func TestFormShowsEveryFieldAndBothKindsIncludingMaxCost(t *testing.T) {
	c := formCockpit(t)
	out := c.form.view(80, 24, c.modelView, nil, nil)
	for _, want := range []string{"name", "kind", "model", "cwd", "max-cost"} {
		if !strings.Contains(out, want) {
			t.Errorf("form view is missing the %q row", want)
		}
	}
}

func TestMaxCostFieldEmptyMeansUnlimited(t *testing.T) {
	c := formCockpit(t)
	p, problem := c.form.params(nil)
	if problem != "" {
		t.Fatalf("params: %v", problem)
	}
	if p.maxCost != nil {
		t.Errorf("maxCost = %v, want nil (empty field = unlimited)", *p.maxCost)
	}
}

func TestMaxCostFieldConvertsThroughCurrency(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("13.80")
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}

	p, problem := c.form.params(cur)
	if problem != "" {
		t.Fatalf("params: %v", problem)
	}
	if p.maxCost == nil {
		t.Fatal("maxCost is nil, want a converted USD value")
	}
	if diff := *p.maxCost - 10.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("maxCost = %v, want ~10.0", *p.maxCost)
	}
}

func TestMaxCostFieldRejectsGarbage(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("not-a-number")

	_, problem := c.form.params(nil)
	if !strings.Contains(problem, "max-cost") {
		t.Errorf("problem = %q, want it to name max-cost", problem)
	}
}

// grantedCost (cmd/rafikid/limits.go) treats a zero MaxCost as UNLIMITED, so
// typing "0" into this field must be rejected rather than silently granting
// unlimited spend — the opposite of what someone typing a budget means.
func TestMaxCostFieldRejectsZero(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("0")

	p, problem := c.form.params(nil)
	if problem == "" {
		t.Fatalf("params accepted 0 as a max-cost: %+v", p)
	}
	if !strings.Contains(problem, "max-cost") || !strings.Contains(problem, "0") {
		t.Errorf("problem = %q, want it to name max-cost and explain 0 means unlimited", problem)
	}
	if p.maxCost != nil {
		t.Errorf("maxCost = %v, want nil on a rejected value", *p.maxCost)
	}
}
