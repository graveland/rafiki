// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// railWith seeds a cockpit's rail with live children and focuses the rail pane.
func railWith(t *testing.T, ids ...string) *Cockpit {
	t.Helper()
	c := newTestCockpit("")
	sums := make([]*rafikiv1.ChildSummary, 0, len(ids))
	for _, id := range ids {
		sums = append(sums, summaryFor(id, id, 0))
	}
	c.rail.Seed(sums)
	c.focus = focusRail
	c.selected = ids[0]
	return c
}

func exitChild(c *Cockpit, id string) {
	code := int32(0)
	c.rail.Apply(&rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_ChildExited{
			ChildExited: &rafikiv1.ChildExited{ExitCode: &code}}})
}

func press(c *Cockpit, key string) tea.Cmd {
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
	return cmd
}

// ── the confirmation ─────────────────────────────────────────────────────────

// x must never end an agent on the first press. This is the whole safety
// property: the rail's keys are bare letters, so a mistyped key lands here.
func TestEndAgentArmsBeforeItActs(t *testing.T) {
	c := railWith(t, "c_1", "c_2")

	if cmd := c.endSelected(); cmd != nil {
		t.Fatal("first x returned a command; it must only arm")
	}
	if c.endArmedID != "c_1" {
		t.Errorf("endArmedID = %q, want c_1", c.endArmedID)
	}
	if !strings.Contains(c.notice, "again") {
		t.Errorf("notice = %q, want it to ask for a repeat", c.notice)
	}
	if cmd := c.endSelected(); cmd == nil {
		t.Error("second x returned no command; the confirmed press must act")
	}
}

// Arming on one agent and moving the cursor must NOT let the repeat end the
// agent now under the cursor -- the confirmation named a different one.
func TestEndAgentArmIsPerChild(t *testing.T) {
	c := railWith(t, "c_1", "c_2")

	c.endSelected() // arms c_1
	c.selected = "c_2"

	if cmd := c.endSelected(); cmd != nil {
		t.Fatal("x on a different agent acted on the arm meant for the first")
	}
	if c.endArmedID != "c_2" {
		t.Errorf("endArmedID = %q, want the arm to move to c_2", c.endArmedID)
	}
}

func TestEndAgentArmExpires(t *testing.T) {
	c := railWith(t, "c_1")
	c.endSelected()
	c.endArmed = time.Now().Add(-2 * quitConfirmWindow)

	if cmd := c.endSelected(); cmd != nil {
		t.Error("a stale arm still acted; it must re-arm instead")
	}
}

// Any other key disarms, so an x left armed minutes ago cannot be completed by
// an unrelated keystroke later.
func TestUnrelatedKeyDisarmsEndAgent(t *testing.T) {
	c := railWith(t, "c_1")
	c.endSelected()

	press(c, "j")

	if c.endArmedID != "" {
		t.Errorf("endArmedID = %q after an unrelated key, want cleared", c.endArmedID)
	}
}

// ── the three outcomes ───────────────────────────────────────────────────────

func TestEndAgentVerbFollowsTheRowsState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Cockpit)
		verb  string
	}{
		{"live child stops", func(*Cockpit) {}, "stop"},
		{"exited child closes", func(c *Cockpit) { exitChild(c, "c_1") }, "close"},
		{"shutting down forces", func(c *Cockpit) {
			c.rail.Apply(&rafikiv1.Event{ChildId: "c_1",
				Payload: &rafikiv1.Event_AgentStatus{
					AgentStatus: &rafikiv1.AgentStatus{State: statusShuttingDown}}})
		}, "force kill"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := railWith(t, "c_1")
			tt.setup(c)
			c.endSelected()
			if !strings.Contains(c.notice, tt.verb) {
				t.Errorf("notice = %q, want it to name %q", c.notice, tt.verb)
			}
		})
	}
}

// ── results ──────────────────────────────────────────────────────────────────

// A kill does NOT remove the row: the child's own child_exited does that, and
// the exit code is worth seeing.
func TestKillLeavesTheRowForTheExitEvent(t *testing.T) {
	c := railWith(t, "c_1")
	c.applyKilled(killedMsg{childID: "c_1", name: "c_1"})

	if _, ok := c.rail.Get("c_1"); !ok {
		t.Error("kill removed the rail row; the exit event owns that")
	}
}

// A close DOES remove it: the child is gone from the daemon's store, so
// nothing will ever publish about it again.
func TestCloseRemovesTheRow(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	exitChild(c, "c_1")
	c.applyClosed(closedMsg{childID: "c_1", name: "c_1"})

	if _, ok := c.rail.Get("c_1"); ok {
		t.Error("closed child still has a rail row; nothing else will ever drop it")
	}
	if c.selected == "c_1" {
		t.Error("selection left on a row that no longer exists")
	}
}

func TestCloseFailureKeepsTheRow(t *testing.T) {
	c := railWith(t, "c_1")
	c.applyClosed(closedMsg{childID: "c_1", name: "c_1", err: errors.New("nope")})

	if _, ok := c.rail.Get("c_1"); !ok {
		t.Error("a FAILED close dropped the row; the child is still there")
	}
	if !strings.Contains(c.notice, "could not close") {
		t.Errorf("notice = %q, want the failure reported", c.notice)
	}
}

func TestKillFailureIsReported(t *testing.T) {
	c := railWith(t, "c_1")
	c.applyKilled(killedMsg{childID: "c_1", name: "alpha",
		err: errors.New("internal: child is still running")})

	if !strings.Contains(c.notice, "alpha") {
		t.Errorf("notice = %q, want it to name the agent", c.notice)
	}
	// connect-go's transport prefix must not reach a one-line notice: the
	// daemon's own sentence is the useful half.
	if strings.Contains(c.notice, "internal:") {
		t.Errorf("notice = %q, want the RPC prefix trimmed", c.notice)
	}
}

func TestTrimRPCErrorKeepsAPrefixlessMessage(t *testing.T) {
	if got := trimRPCError(errors.New("boom")); got != "boom" {
		t.Errorf("trimRPCError = %q, want boom", got)
	}
}

// ── buildSpawnRequest: the form's kind-aware executor rule ───────────────────

// An explicit executor field always wins, and it rides ExecutorRef alone —
// a ref and a selector on the same request would answer two different
// questions about where the child runs.
func TestBuildSpawnRequestPrefersExplicitExecutorField(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp", executor: "greyshift"})
	if req.GetExecutorRef() != "greyshift" {
		t.Fatalf("want ExecutorRef=greyshift, got %q", req.GetExecutorRef())
	}
	if req.GetExecutorSelector() != "" {
		t.Fatalf("ExecutorRef and ExecutorSelector must be mutually exclusive here, got selector=%q", req.GetExecutorSelector())
	}
}

// fundi with a blank field keeps its historical default: the session executor
// this cockpit was built with.
func TestBuildSpawnRequestFundiUsesTheSessionExecutorWhenFieldIsBlank(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "fundi", cwd: "/tmp"})
	if req.GetExecutorSelector() != "owner=brent,machine=silvershift" {
		t.Fatalf("want the session executor's selector for fundi, got %q", req.GetExecutorSelector())
	}
	if req.GetExecutorRef() != "" {
		t.Fatalf("want no ref, got %q", req.GetExecutorRef())
	}
}

// A launch-required kind (anything but fundi) with a blank field sends NEITHER
// field: the local session executor can never launch anything -- see
// startSessionExecutor and docs/plans/2026-09-06-executor-selection-design.md
// §1. Leaving both empty lets the daemon's chooseLaunchExecutor auto-resolve
// across every durable executor, not just this machine's.
func TestBuildSpawnRequestClaudeWithBlankFieldLeavesBothEmpty(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp"})
	if req.GetExecutorSelector() != "" || req.GetExecutorRef() != "" {
		t.Fatalf("want both empty for an unspecified claude executor, got selector=%q ref=%q",
			req.GetExecutorSelector(), req.GetExecutorRef())
	}
}

// fundi WITH an explicit field sends the ref, not the session selector — the
// explicit choice outranks the default the same way it does for claude.
func TestBuildSpawnRequestFundiWithExplicitFieldSendsTheRef(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "fundi", cwd: "/tmp", executor: "greyshift"})
	if req.GetExecutorRef() != "greyshift" {
		t.Fatalf("want ExecutorRef=greyshift, got %q", req.GetExecutorRef())
	}
	if req.GetExecutorSelector() != "" {
		t.Fatalf("want no selector, got %q", req.GetExecutorSelector())
	}
}

// A DECLARED --executor-selector is a policy for every kind: the flag branch
// of `rafiki create` honors it for launch-required kinds, and the form path
// must not silently drop it. It is the SESSION executor's selector that is
// fundi-only, and executorSelectorFromFlag is what tells the two apart.
func TestBuildSpawnRequestAppliesAFlagSelectorToALaunchKind(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,env=prod", executorSelectorFromFlag: true}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp"})
	if req.GetExecutorSelector() != "owner=brent,env=prod" {
		t.Fatalf("want the declared selector preserved for claude, got %q", req.GetExecutorSelector())
	}
	if req.GetExecutorRef() != "" {
		t.Fatalf("want no ref, got %q", req.GetExecutorRef())
	}
}

// The session selector must NEVER pin a launch-required kind, even though the
// field is non-empty -- that is the bug this whole plan exists to fix.
func TestBuildSpawnRequestSessionSelectorNeverPinsALaunchKind(t *testing.T) {
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift", executorSelectorFromFlag: false}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp"})
	if req.GetExecutorSelector() != "" || req.GetExecutorRef() != "" {
		t.Fatalf("want both empty, got selector=%q ref=%q", req.GetExecutorSelector(), req.GetExecutorRef())
	}
}

// ── budget edit modal ────────────────────────────────────────────────────────

func TestEditBudgetOpensModalPrefilled(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	maxCost := 15.5
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha", Status: "idle", Labels: map[string]string{}, MaxCost: &maxCost},
		{ChildId: "c_2", Name: "beta", Status: "idle", Labels: map[string]string{}},
	})
	c.focus = focusRail
	c.selected = "c_1"

	press(c, "b")

	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil after pressing 'b' on rail")
	}
	if c.budgetForm.childID != "c_1" {
		t.Errorf("childID = %q, want c_1", c.budgetForm.childID)
	}
	if c.budgetForm.childName != "alpha" {
		t.Errorf("childName = %q, want alpha", c.budgetForm.childName)
	}
	if got := c.budgetForm.input.Value(); got != "15.5" {
		t.Errorf("prefilled value = %q, want 15.5", got)
	}

	// For child without MaxCost (0 / unlimited), the field should be blank.
	c.selected = "c_2"
	c.budgetForm = nil
	press(c, "b")
	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil for c_2")
	}
	if got := c.budgetForm.input.Value(); got != "" {
		t.Errorf("prefilled value for unlimited child = %q, want empty", got)
	}
}

func TestBudgetFormRejectsNegativeAmount(t *testing.T) {
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil")
	}

	c.budgetForm.input.SetValue("-5.00")
	// Press enter on the modal
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmd != nil {
		t.Error("cmd returned on invalid negative budget, want nil")
	}
	if c.budgetForm == nil {
		t.Fatal("budgetForm dismissed on invalid input")
	}
	if c.budgetForm.err == "" {
		t.Error("expected error on negative budget, got empty err")
	}
	if c.budgetForm.busy {
		t.Error("busy is true on validation error")
	}
}

func TestBudgetFormAcceptsZeroAndBlankAsUnlimited(t *testing.T) {
	c := railWith(t, "c_1")
	c.selected = "c_1"

	// 1. Blank field
	press(c, "b")
	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil")
	}
	c.budgetForm.input.SetValue("")
	maxCost, problem := c.budgetForm.params(c.currency)
	if problem != "" {
		t.Errorf("unexpected problem for blank field: %s", problem)
	}
	if maxCost != 0 {
		t.Errorf("maxCost = %v, want 0 for blank", maxCost)
	}
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmd == nil {
		t.Error("expected cmd returned on enter for blank field")
	}
	if !c.budgetForm.busy {
		t.Error("busy should be true after submission")
	}

	// 2. Explicit "0"
	c.budgetForm = nil
	press(c, "b")
	c.budgetForm.input.SetValue("0")
	maxCost, problem = c.budgetForm.params(c.currency)
	if problem != "" {
		t.Errorf("unexpected problem for explicit '0': %s", problem)
	}
	if maxCost != 0 {
		t.Errorf("maxCost = %v, want 0 for '0'", maxCost)
	}
	_, cmd = c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmd == nil {
		t.Error("expected cmd returned on enter for '0'")
	}
}

func TestApplyBudgetSetUpdatesRailAndDismissesModal(t *testing.T) {
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil")
	}

	origin := c.budgetForm
	c.applyBudgetSet(budgetSetMsg{origin: origin, childID: "c_1", name: "c_1", maxCost: 42.0, err: nil})
	if c.budgetForm != nil {
		t.Error("budgetForm not dismissed after successful applyBudgetSet")
	}
	node, ok := c.rail.Get("c_1")
	if !ok {
		t.Fatal("c_1 missing from rail")
	}
	if node.MaxCost != 42.0 {
		t.Errorf("MaxCost = %v, want 42.0", node.MaxCost)
	}
	if !strings.Contains(c.notice, "budget set for c_1") {
		t.Errorf("notice = %q, want 'budget set for c_1'", c.notice)
	}

	// Test clearing budget (maxCost == 0)
	press(c, "b")
	origin = c.budgetForm
	c.applyBudgetSet(budgetSetMsg{origin: origin, childID: "c_1", name: "c_1", maxCost: 0, err: nil})
	node, _ = c.rail.Get("c_1")
	if node.MaxCost != 0 {
		t.Errorf("MaxCost = %v, want 0 after clear", node.MaxCost)
	}
	if !strings.Contains(c.notice, "budget cleared for c_1") {
		t.Errorf("notice = %q, want 'budget cleared for c_1'", c.notice)
	}
}

func TestBudgetFormFailureResetsBusyAndSurfacesError(t *testing.T) {
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	if c.budgetForm == nil {
		t.Fatal("budgetForm is nil")
	}
	f := c.budgetForm
	f.input.SetValue("10")
	// Submit
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmd == nil {
		t.Fatal("cmd is nil on enter")
	}
	if !f.busy {
		t.Fatal("f.busy should be true after submit")
	}

	// Simulate RPC failure
	rpcErr := errors.New("rpc error: code = InvalidArgument desc = budget exceeds parent")
	c.applyBudgetSet(budgetSetMsg{origin: f, childID: "c_1", name: "c_1", maxCost: 10, err: rpcErr})

	if c.budgetForm == nil {
		t.Fatal("modal should still be open after failure")
	}
	if f.busy {
		t.Error("busy should be false after failure so user can retry")
	}
	if f.err != "code = InvalidArgument desc = budget exceeds parent" {
		t.Errorf("f.err = %q, want 'code = InvalidArgument desc = budget exceeds parent'", f.err)
	}
	if !strings.Contains(c.notice, "code = InvalidArgument desc = budget exceeds parent") {
		t.Errorf("notice = %q, want failure notice", c.notice)
	}

	// Enter can be pressed again to dispatch a new command
	_, cmd2 := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmd2 == nil {
		t.Error("enter should dispatch new command on retry")
	}
	if !f.busy {
		t.Error("busy should be true after second submit")
	}
}

func TestBudgetFormCrossModalCorrelation(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	c.selected = "c_1"
	press(c, "b")
	formA := c.budgetForm
	formA.input.SetValue("25")
	_, cmdA := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	if cmdA == nil {
		t.Fatal("cmdA is nil")
	}

	// Operator cancels form A (esc clears c.budgetForm) and switches to child c_2 modal while request A is in flight
	press(c, "esc")
	if c.budgetForm != nil {
		t.Fatal("budgetForm should be nil after esc")
	}
	c.selected = "c_2"
	press(c, "b")
	formB := c.budgetForm
	if formB == nil || formB == formA {
		t.Fatal("formB should be a new form for c_2")
	}

	// Now deliver A's delayed success message
	c.applyBudgetSet(budgetSetMsg{origin: formA, childID: "c_1", name: "c_1", maxCost: 25.0, err: nil})

	// Form B should STILL be open
	if c.budgetForm != formB {
		t.Errorf("c.budgetForm was dismissed or replaced, want formB to stay open")
	}

	// Rail for c_1 should be updated to 25.0
	nodeA, ok := c.rail.Get("c_1")
	if !ok || nodeA.MaxCost != 25.0 {
		t.Errorf("c_1 MaxCost = %v, want 25.0", nodeA.MaxCost)
	}

	// Deliver A's delayed failure to test failure path with different active modal
	formA.busy = true
	c.applyBudgetSet(budgetSetMsg{origin: formA, childID: "c_1", name: "c_1", maxCost: 25.0, err: errors.New("fail")})
	// Form B must not receive form A's error
	if formB.err != "" {
		t.Errorf("formB received formA's error: %q", formB.err)
	}
}

func TestBudgetFormPlaceholderRenderWidth(t *testing.T) {
	form := newBudgetForm("c_1", "worker", 0, nil)
	rendered := form.view(80, 24, nil)
	stripped := ansi.Strip(rendered)
	if !strings.Contains(stripped, "(unlimited)") {
		t.Errorf("rendered view missing full placeholder '(unlimited)':\n%s", stripped)
	}
}
