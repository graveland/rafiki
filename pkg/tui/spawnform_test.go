// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NotNil(c.form, "n on the agents pane did not open the create form")
	return c
}

// The model row moved to the END of the tab order: tabbing to set a cost cap
// must not require passing through the model row first, and the executor row
// (added with this test) sits beside kind because both decide where/how a
// child runs while the model only decides what it runs.
func TestModelIsTheLastFormField(t *testing.T) {
	assert.NewAborting(t).Eq(spawnFieldCount-1, fieldModel, "fieldModel must be the last field (index")
}

// The executor row feeds SpawnRequest.ExecutorRef: a machine name or id naming
// one specific executor, blank meaning the kind-aware auto-resolve.
func TestSpawnParamsCarriesExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSpawnForm()
	f.inputs[fieldExecutor].SetValue("greyshift")
	f.inputs[fieldCwd].SetValue("/tmp")
	p, problem := f.params(nil)
	c.Eq("", problem)
	c.Eq("greyshift", p.executor, "want executor=greyshift, got")
}

// A spawn's defaults carry an executor the same way they carry a model, so
// `rafiki create -i --executor greyshift` shows what it is about to target.
func TestOpenCreatePrefillsTheExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewCockpit(Options{
		BaseURL:    "http://127.0.0.1:1",
		OpenCreate: true,
		CreateDefaults: SpawnDefaults{
			Name: "reviewer", Kind: "claude", Executor: "greyshift", Cwd: "/tmp/x",
		},
	})
	ck.Require().NotNil(c.form, "OpenCreate did not open the form")
	ck.Eq("greyshift", c.form.inputs[fieldExecutor].Value(), "executor")
}

// A preset named by the caller rides every spawn the form issues — it is not
// an editable field, so nothing is shown and nothing the user types can drop
// it; buildSpawnRequest is what puts it on the wire.
func TestPresetDefaultsRideTheFormWithoutBeingAField(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewCockpit(Options{
		BaseURL:    "http://127.0.0.1:1",
		OpenCreate: true,
		CreateDefaults: SpawnDefaults{
			Name: "reviewer", Kind: "claude", Preset: "reviewer", Cwd: "/tmp/x",
		},
	})
	ck.Require().NotNil(c.form, "OpenCreate did not open the form")
	sp, errMsg := c.form.params(nil)
	ck.Require().Eq("", errMsg, "form params")
	req := c.buildSpawnRequest(sp)
	ck.Eq("reviewer", req.GetPreset(), "Preset")
	// The established contract: with a preset the model prefill stays empty —
	// the daemon resolves the preset's model.
	ck.Eq("", c.form.inputs[fieldModel].Value(), "model prefill")
}

func TestNOpensTheCreateForm(t *testing.T) {
	formCockpit(t)
}

// n must do nothing from the input pane: it is a letter, and typing "now" into
// a prompt must not spawn an agent.
func TestNInTheInputPaneIsJustALetter(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.focus = focusInput
	c.handleKey(keyMsg("n"))

	ck.Require().Nil(c.form, "n opened the create form while typing")
	ck.StrContains(c.ta.Value(), "n", "textarea")
}

// The modal is checked BEFORE the globals, so tab must move between fields
// rather than reaching cyclePane.
func TestTabCyclesFieldsRatherThanPanes(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	before := c.focus

	c.handleKey(keyMsg("tab"))

	ck.Eq(fieldKind, c.form.focus, "form focus")
	ck.Eq(before, c.focus, "tab reached the pane ring; a modal must own that key")
}

func TestFieldFocusWrapsBothWays(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.handleKey(keyMsg("shift+tab"))
	ck.Eq(spawnFieldCount-1, c.form.focus, "focus")
	c.handleKey(keyMsg("tab"))
	ck.Eq(fieldName, c.form.focus, "focus")
}

func TestKindCyclesAndIsNeverFreeText(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.focus = fieldKind

	first := c.form.kind()
	c.handleKey(keyMsg("right"))
	ck.Require().NotEq(first, c.form.kind(), "→ did not change the kind")
	c.handleKey(keyMsg("left"))
	ck.Eq(first, c.form.kind(), "← did not restore the kind; the cycle must be symmetric")

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

	assert.NewCollecting(t).NotEq(first, c.form.kind(), "space did not cycle the kind; still")
}

func TestEscapeCancelsTheForm(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("esc"))
	assert.NewCollecting(t).Nil(c.form, "esc did not close the form")
}

// ^C in a modal cancels the modal. It must NOT arm the cockpit's quit.
func TestCtrlCCancelsTheFormWithoutArmingQuit(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	ck.Nil(c.form, "^C did not close the form")
	ck.True(c.quitArmed.IsZero(), "^C armed quit from inside a modal")
}

// cwd is required by the server. Catching it here keeps the values on screen
// instead of spending a round trip to be told.
func TestEmptyCwdIsRefusedLocally(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.inputs[fieldCwd].SetValue("")

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.Nil(cmd, "submitted with no cwd; the server would refuse it")
	ck.Require().NotNil(c.form, "form closed on a validation failure")
	ck.StrContains(c.form.err, "cwd", "err")
}

func TestCwdIsPrefilled(t *testing.T) {
	c := formCockpit(t)
	assert.NewCollecting(t).NotEq("", c.form.inputs[fieldCwd].Value(), "cwd was not prefilled; every create would need it typed by hand")
}

// A refused spawn keeps the form and its values: the daemon's complaint is
// usually about one field, and dismissing throws away what needs correcting.
func TestSpawnFailureKeepsTheFormAndItsValues(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.inputs[fieldName].SetValue("scout")
	c.form.busy = true

	c.applySpawned(spawnedMsg{err: errors.New("internal: no such directory")})

	ck.Require().NotNil(c.form, "a failed spawn dismissed the form")
	ck.Eq("scout", c.form.inputs[fieldName].Value(), "the typed name was lost")
	ck.False(c.form.busy, "form still busy after a failure; a retry would be impossible")
	ck.StrContains(c.form.err, "no such directory", "err")
}

func TestSpawnSuccessClosesTheForm(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	c.applySpawned(spawnedMsg{childID: "c_new"})

	assert.NewCollecting(t).Nil(c.form, "form stayed open after a successful create")
}

// A second Enter while the first is in flight must not create two agents.
func TestBusyFormRefusesASecondSubmit(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	_, cmd := c.handleKey(keyMsg("enter"))
	assert.NewCollecting(t).Nil(cmd, "a busy form submitted again")
}

// The modal must render instead of the transcript, and ahead of the help sheet.
func TestFormOwnsTheBodyPane(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.showHelp = true
	c.width, c.height, c.ready = 100, 30, true

	out := ansi.Strip(c.View().Content)
	ck.StrContains(out, "new agent", "the form did not render")
	ck.NotStrContains(out, "closes this", "the help sheet rendered over the modal")
}

func TestFormShowsEveryFieldAndBothKinds(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	out := c.form.view(80, 24, c.modelView, nil, nil)
	for _, want := range []string{"name", "kind", "model", "cwd"} {
		ck.StrContains(out, want, "form view is missing the")
	}
	// Both kinds are shown, not just the selected one: a single value gives no
	// hint that the row can change.
	for _, k := range spawnKinds {
		ck.StrContains(out, k, "form view does not offer kind")
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

	assert.NewAborting(t).Eq("scout", c.form.inputs[fieldName].Value(), "name field")
}

// Focus must MOVE with the tab order, not stay on the first field. Blurring the
// old row and focusing the new one are two separate calls, and getting only the
// first right yields a form where every row after the first is dead.
func TestTypingFollowsTheFocusedField(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	// Walk by the named constant, not a tab count: the tab order changed when
	// the executor row joined (model moved last), and a count silently became
	// the wrong row.
	for c.form.focus != fieldModel {
		c.handleKey(keyMsg("tab"))
	}

	for _, r := range "opus" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	ck.Eq("opus", c.form.inputs[fieldModel].Value(), "model field")
	ck.Eq("", c.form.inputs[fieldName].Value(), "name field")
}

// A modal takes the WHOLE panel: a rail behind the create form is a list you
// cannot act on, costing width from a table that needs it.
func TestModalsHideTheRail(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	c.width, c.height, c.ready = 100, 30, true
	ck.Require().NotEq(0, c.railCols(), "no rail to begin with")
	before := c.convWidth()

	c.handleKey(keyMsg("n"))

	ck.Eq(0, c.railCols(), "the rail is still drawn behind the create form")
	ck.Greater(before, c.convWidth(), "the form did not get the width the rail gave up")
	ck.NotStrContains(ansi.Strip(c.View().Content), "c_2", "a rail row rendered behind the modal")

	c.handleKey(keyMsg("esc"))
	ck.NotEq(0, c.railCols(), "the rail did not come back when the modal closed")
}

// `rafiki create` with nothing to go on opens straight into the form, prefilled
// with what a bare create would have spawned — so the default case costs one ⏎
// and shows what it is about to do.
func TestOpenCreatePrefillsTheForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewCockpit(Options{
		BaseURL:    "http://127.0.0.1:1",
		OpenCreate: true,
		CreateDefaults: SpawnDefaults{
			Name: "reviewer", Kind: "claude", Model: "anthropic/claude-opus-5", Cwd: "/tmp/x",
		},
	})
	ck.Require().NotNil(c.form, "OpenCreate did not open the form")
	ck.Eq("reviewer", c.form.inputs[fieldName].Value(), "name =")
	ck.Eq("claude", c.form.kind(), "kind")
	ck.Eq("anthropic/claude-opus-5", c.form.inputs[fieldModel].Value(), "model =")
	ck.Eq("/tmp/x", c.form.inputs[fieldCwd].Value(), "cwd =")
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
	assert.NewCollecting(t).Eq("owner=brent", c.executorSelector, "executorSelector")
}

// Empty defaults keep the form's own, rather than blanking the prefilled cwd.
func TestOpenCreateWithNoDefaultsKeepsTheFormsOwn(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", OpenCreate: true})
	ck.Require().NotNil(c.form, "OpenCreate did not open the form")
	ck.NotEq("", c.form.inputs[fieldCwd].Value(), "cwd prefill was cleared by an empty default")
}

// A form opened at CONSTRUCTION never saw the `n` keypress that normally starts
// the catalog fetch, so Init has to start it or the typeahead sits empty.
func TestOpenCreateFetchesTheCatalog(t *testing.T) {
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", OpenCreate: true})
	_ = c.Init()
	assert.NewCollecting(t).False(!c.modelsBusy[c.form.kind()], "no catalog fetch was started for a form opened at construction")
}

// bubbles renders a ONE-character placeholder when Width is unset:
// placeholderView sizes its buffer to Width()+1, copies the placeholder in,
// and early-returns having emitted only p[:1]. "(auto)" came out as "(" and
// the picker's "filter…" as "f", in shipped output nobody read closely.
func TestPlaceholdersRenderInFull(t *testing.T) {
	c := formCockpit(t)
	out := ansi.Strip(c.form.view(90, 24, c.modelView, nil, nil))
	for _, want := range []string{"(auto)", "(daemon default)"} {
		assert.NewCollecting(t).StrContains(out, want, "placeholder")
	}
}

func TestPickerFilterPlaceholderRendersInFull(t *testing.T) {
	c, p := loadedPicker(t)
	p.filter.SetValue("")
	out := ansi.Strip(p.view(90, 20, c.modelView, nil))
	assert.NewCollecting(t).StrContains(out, "filter…", "the filter placeholder is truncated:\n")
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
	assert.NewCollecting(t).StrContains(out, "anthropic/claude-opus-5", "the typed model id is not shown in full:\n")
}

func TestFormShowsEveryFieldAndBothKindsIncludingMaxCost(t *testing.T) {
	c := formCockpit(t)
	out := c.form.view(80, 24, c.modelView, nil, nil)
	for _, want := range []string{"name", "kind", "model", "cwd", "max-cost"} {
		assert.NewCollecting(t).StrContains(out, want, "form view is missing the")
	}
}

func TestMaxCostFieldEmptyMeansUnlimited(t *testing.T) {
	c := formCockpit(t)
	p, problem := c.form.params(nil)
	assert.NewAborting(t).Eq("", problem, "params")
	if p.maxCost != nil {
		t.Errorf("maxCost = %v, want nil (empty field = unlimited)", *p.maxCost)
	}
}

func TestMaxCostFieldConvertsThroughCurrency(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("13.80")
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}

	p, problem := c.form.params(cur)
	ck.Eq("", problem, "params")
	ck.NotNil(p.maxCost, "maxCost is nil, want a converted USD value")
	if diff := *p.maxCost - 10.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("maxCost = %v, want ~10.0", *p.maxCost)
	}
}

func TestMaxCostFieldRejectsGarbage(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("not-a-number")

	_, problem := c.form.params(nil)
	assert.NewCollecting(t).StrContains(problem, "max-cost", "problem")
}

// grantedCost (cmd/rafikid/limits.go) treats a zero MaxCost as UNLIMITED, so
// typing "0" into this field must be rejected rather than silently granting
// unlimited spend — the opposite of what someone typing a budget means.
func TestMaxCostFieldRejectsZero(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.inputs[fieldMaxCost].SetValue("0")

	p, problem := c.form.params(nil)
	ck.Require().NotEq("", problem, "params accepted 0 as a max-cost: %+v", p)
	ck.False(!strings.Contains(problem, "max-cost") || !strings.Contains(problem, "0"), "problem = %q, want it to name max-cost and explain 0 means unlimited", problem)
	if p.maxCost != nil {
		t.Errorf("maxCost = %v, want nil on a rejected value", *p.maxCost)
	}
}

// Submitting a launch-required kind with nothing chosen and several eligible
// executors must ASK, not let the daemon guess: the picker auto-opens with the
// cached eligible rows and the form stays open, un-busy, ready to submit after
// a pick.
func TestSubmitWithAmbiguousExecutorOpensThePicker(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.kindIx = 1 // claude — see spawnKinds
	c.executors = map[string][]*rafikiv1.ExecutorRow{
		"claude": {
			{Id: "exec-1", Machine: "greyshift", Eligible: true},
			{Id: "exec-2", Machine: "otherbox", Eligible: true},
		},
	}

	c.handleKey(keyMsg("enter"))

	ck.Require().NotNil(c.execPicker, "want the picker to auto-open on ambiguity")
	ck.Require().Len(c.execPicker.rows, 2, "want both eligible rows offered, got %d", len(c.execPicker.rows))
	ck.False(c.form.busy, "the form went busy on a submit it intercepted")
	ck.Eq("", c.form.err, "unexpected form error")
}

// One eligible executor is not ambiguous: no picker, straight through to the
// spawn.
func TestSubmitWithOneEligibleExecutorSubmitsDirectly(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.form.kindIx = 1
	c.executors = map[string][]*rafikiv1.ExecutorRow{
		"claude": {{Id: "exec-1", Machine: "greyshift", Eligible: true}},
	}

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.Require().NotNil(cmd, "a unambiguous submit did not spawn")
	ck.Require().Nil(c.execPicker, "the picker opened for a single eligible executor")
	ck.True(c.form.busy, "the form is not busy on an in-flight spawn")
}

// An explicit executor choice is never second-guessed, even with several
// eligible rows cached.
func TestSubmitWithExplicitExecutorSkipsTheAmbiguityCheck(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.form.kindIx = 1
	c.form.inputs[fieldExecutor].SetValue("greyshift")
	c.executors = map[string][]*rafikiv1.ExecutorRow{
		"claude": {
			{Id: "exec-1", Machine: "greyshift", Eligible: true},
			{Id: "exec-2", Machine: "otherbox", Eligible: true},
		},
	}

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.NotNil(cmd, "an explicit executor did not spawn")
	ck.Nil(c.execPicker, "the picker opened over an explicit choice")
}

// fundi never gets the check: its historical default is the session executor,
// which the daemon-side narrowing handles.
func TestSubmitFundiSkipsTheAmbiguityCheck(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.executors = map[string][]*rafikiv1.ExecutorRow{
		"fundi": {
			{Id: "exec-1", Machine: "a", Eligible: true},
			{Id: "exec-2", Machine: "b", Eligible: true},
		},
	}

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.NotNil(cmd, "a fundi submit did not spawn")
	ck.Nil(c.execPicker, "the picker opened for fundi")
}

// A kind change swaps in that kind's remembered executor and warms both
// caches -- the ambiguity check reads only cached rows, so the kind change is
// its proactive fetch.
func TestKindChangeSwapsTheRememberedExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.profileName = "work"
	clientstate.RememberExecutor("work", "claude", "greyshift")
	c.form.kindIx = 1

	c.kindChanged()

	ck.Require().Eq("greyshift", c.form.inputs[fieldExecutor].Value(), "executor field")
	ck.False(!c.executorsBusy["claude"], "no executor fetch was issued for the new kind")
	ck.False(!c.modelsBusy["claude"], "no model fetch was issued for the new kind")
}

// A declared --executor-selector is already a decision; the daemon resolves it
// silently (documented first-match), and re-asking over it is noise.
func TestSubmitWithFlagSelectorSkipsTheAmbiguityCheck(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.form.kindIx = 1
	c.executorSelector = "owner=brent,env=prod"
	c.executorSelectorFromFlag = true
	c.executors = map[string][]*rafikiv1.ExecutorRow{
		"claude": {
			{Id: "exec-1", Machine: "greyshift", Eligible: true},
			{Id: "exec-2", Machine: "otherbox", Eligible: true},
		},
	}

	_, cmd := c.handleKey(keyMsg("enter"))

	ck.NotNil(cmd, "a declared selector did not spawn")
	ck.Nil(c.execPicker, "the picker opened over a declared selector policy")
}
