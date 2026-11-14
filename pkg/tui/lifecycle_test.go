// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
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
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")

	ck.Require().Nil(c.endSelected(), "first x returned a command; it must only arm")
	ck.Eq("c_1", c.endArmedID, "endArmedID")
	ck.StrContains(c.notice, "again", "notice")
	ck.NotNil(c.endSelected(), "second x returned no command; the confirmed press must act")
}

// Arming on one agent and moving the cursor must NOT let the repeat end the
// agent now under the cursor -- the confirmation named a different one.
func TestEndAgentArmIsPerChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")

	c.endSelected() // arms c_1
	c.selected = "c_2"

	ck.Require().Nil(c.endSelected(), "x on a different agent acted on the arm meant for the first")
	ck.Eq("c_2", c.endArmedID, "endArmedID")
}

func TestEndAgentArmExpires(t *testing.T) {
	c := railWith(t, "c_1")
	c.endSelected()
	c.endArmed = time.Now().Add(-2 * quitConfirmWindow)

	assert.NewCollecting(t).Nil(c.endSelected(), "a stale arm still acted; it must re-arm instead")
}

// Any other key disarms, so an x left armed minutes ago cannot be completed by
// an unrelated keystroke later.
func TestUnrelatedKeyDisarmsEndAgent(t *testing.T) {
	c := railWith(t, "c_1")
	c.endSelected()

	press(c, "j")

	assert.NewCollecting(t).Eq("", c.endArmedID, "endArmedID")
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
			assert.NewCollecting(t).StrContains(c.notice, tt.verb, "notice")
		})
	}
}

// ── results ──────────────────────────────────────────────────────────────────

// A kill does NOT remove the row: the child's own child_exited does that, and
// the exit code is worth seeing.
func TestKillLeavesTheRowForTheExitEvent(t *testing.T) {
	c := railWith(t, "c_1")
	c.applyKilled(killedMsg{childID: "c_1", name: "c_1"})

	_, ok := c.rail.Get("c_1")
	assert.NewCollecting(t).True(ok, "kill removed the rail row; the exit event owns that")
}

// A close DOES remove it: the child is gone from the daemon's store, so
// nothing will ever publish about it again.
func TestCloseRemovesTheRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	exitChild(c, "c_1")
	c.applyClosed(closedMsg{childID: "c_1", name: "c_1"})

	_, ok := c.rail.Get("c_1")
	ck.False(ok, "closed child still has a rail row; nothing else will ever drop it")
	ck.NotEq("c_1", c.selected, "selection left on a row that no longer exists")
}

// Closing the viewed row moves the cursor to its successor, and the pane has to
// follow without another keypress: the subscription is re-pointed at the
// successor (here, its history fetch is issued) rather than left on the dead id.
func TestCloseRepointsTheViewAtTheSuccessor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	c.rail.SetFocus("c_1")
	c.focusChild = "c_1"
	c.applyClosed(closedMsg{childID: "c_1", name: "c_1"})

	ck.Eq("c_2", c.selected, "selected")
	ck.Eq("c_2", c.viewing(), "viewing")
	ck.True(c.historyInFlight["c_2"], "successor's transcript was never requested; pane stays empty until the user arrows")
	ck.NotEq("c_1", c.focusChild, "subscription still owned by the closed child")
}

func TestCloseFailureKeepsTheRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.applyClosed(closedMsg{childID: "c_1", name: "c_1", err: errors.New("nope")})

	_, ok := c.rail.Get("c_1")
	ck.True(ok, "a FAILED close dropped the row; the child is still there")
	ck.StrContains(c.notice, "could not close", "notice")
}

func TestKillFailureIsReported(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.applyKilled(killedMsg{childID: "c_1", name: "alpha",
		err: errors.New("internal: child is still running")})

	ck.StrContains(c.notice, "alpha", "notice")
	// connect-go's transport prefix must not reach a one-line notice: the
	// daemon's own sentence is the useful half.
	ck.NotStrContains(c.notice, "internal:", "notice")
}

func TestTrimRPCErrorKeepsAPrefixlessMessage(t *testing.T) {
	assert.NewCollecting(t).Eq("boom", trimRPCError(errors.New("boom")), "trimRPCError")
}

// ── buildSpawnRequest: the form's kind-aware executor rule ───────────────────

// An explicit executor field always wins, and it rides ExecutorRef alone —
// a ref and a selector on the same request would answer two different
// questions about where the child runs.
func TestBuildSpawnRequestPrefersExplicitExecutorField(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp", executor: "greyshift"})
	ck.Eq("greyshift", req.GetExecutorRef(), "want ExecutorRef=greyshift, got")
	ck.Eq("", req.GetExecutorSelector(), "ExecutorRef and ExecutorSelector must be mutually exclusive here, got selector=")
}

// fundi with a blank field keeps its historical default: the session executor
// this cockpit was built with.
func TestBuildSpawnRequestFundiUsesTheSessionExecutorWhenFieldIsBlank(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "fundi", cwd: "/tmp"})
	ck.Eq("owner=brent,machine=silvershift", req.GetExecutorSelector(), "want the session executor's selector for fundi, got")
	ck.Eq("", req.GetExecutorRef(), "want no ref, got")
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
	ck := assert.NewAborting(t)
	c := &Cockpit{executorSelector: "owner=brent,machine=silvershift"}
	req := c.buildSpawnRequest(spawnParams{kind: "fundi", cwd: "/tmp", executor: "greyshift"})
	ck.Eq("greyshift", req.GetExecutorRef(), "want ExecutorRef=greyshift, got")
	ck.Eq("", req.GetExecutorSelector(), "want no selector, got")
}

// A DECLARED --executor-selector is a policy for every kind: the flag branch
// of `rafiki create` honors it for launch-required kinds, and the form path
// must not silently drop it. It is the SESSION executor's selector that is
// fundi-only, and executorSelectorFromFlag is what tells the two apart.
func TestBuildSpawnRequestAppliesAFlagSelectorToALaunchKind(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Cockpit{executorSelector: "owner=brent,env=prod", executorSelectorFromFlag: true}
	req := c.buildSpawnRequest(spawnParams{kind: "claude", cwd: "/tmp"})
	ck.Eq("owner=brent,env=prod", req.GetExecutorSelector(), "want the declared selector preserved for claude, got")
	ck.Eq("", req.GetExecutorRef(), "want no ref, got")
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
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	maxCost := 15.5
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha", Status: "idle", Labels: map[string]string{}, MaxCost: &maxCost},
		{ChildId: "c_2", Name: "beta", Status: "idle", Labels: map[string]string{}},
	})
	c.focus = focusRail
	c.selected = "c_1"

	press(c, "b")

	ck.Require().NotNil(c.budgetForm, "budgetForm is nil after pressing 'b' on rail")
	ck.Eq("c_1", c.budgetForm.childID, "childID")
	ck.Eq("alpha", c.budgetForm.childName, "childName")
	ck.Eq("15.5", c.budgetForm.input.Value(), "prefilled value")

	// For child without MaxCost (0 / unlimited), the field should be blank.
	c.selected = "c_2"
	c.budgetForm = nil
	press(c, "b")
	ck.Require().NotNil(c.budgetForm, "budgetForm is nil for c_2")
	ck.Eq("", c.budgetForm.input.Value(), "prefilled value for unlimited child")
}

func TestBudgetFormRejectsNegativeAmount(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	ck.Require().NotNil(c.budgetForm, "budgetForm is nil")

	c.budgetForm.input.SetValue("-5.00")
	// Press enter on the modal
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.Nil(cmd, "cmd returned on invalid negative budget, want nil")
	ck.Require().NotNil(c.budgetForm, "budgetForm dismissed on invalid input")
	ck.NotEq("", c.budgetForm.err, "expected error on negative budget, got empty err")
	ck.False(c.budgetForm.busy, "busy is true on validation error")
}

func TestBudgetFormAcceptsZeroAndBlankAsUnlimited(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.selected = "c_1"

	// 1. Blank field
	press(c, "b")
	ck.Require().NotNil(c.budgetForm, "budgetForm is nil")
	c.budgetForm.input.SetValue("")
	maxCost, problem := c.budgetForm.params(c.currency)
	ck.Eq("", problem, "unexpected problem for blank field")
	ck.Eq(0, maxCost, "maxCost")
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.NotNil(cmd, "expected cmd returned on enter for blank field")
	ck.True(c.budgetForm.busy, "busy should be true after submission")

	// 2. Explicit "0"
	c.budgetForm = nil
	press(c, "b")
	c.budgetForm.input.SetValue("0")
	maxCost, problem = c.budgetForm.params(c.currency)
	ck.Eq("", problem, "unexpected problem for explicit '0'")
	ck.Eq(0, maxCost, "maxCost")
	_, cmd = c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.NotNil(cmd, "expected cmd returned on enter for '0'")
}

func TestApplyBudgetSetUpdatesRailAndDismissesModal(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	ck.Require().NotNil(c.budgetForm, "budgetForm is nil")

	origin := c.budgetForm
	c.applyBudgetSet(budgetSetMsg{origin: origin, childID: "c_1", name: "c_1", maxCost: 42.0, err: nil})
	ck.Nil(c.budgetForm, "budgetForm not dismissed after successful applyBudgetSet")
	node, ok := c.rail.Get("c_1")
	ck.Require().True(ok, "c_1 missing from rail")
	ck.Eq(42.0, node.MaxCost, "MaxCost")
	ck.StrContains(c.notice, "budget set for c_1", "notice")

	// Test clearing budget (maxCost == 0)
	press(c, "b")
	origin = c.budgetForm
	c.applyBudgetSet(budgetSetMsg{origin: origin, childID: "c_1", name: "c_1", maxCost: 0, err: nil})
	node, _ = c.rail.Get("c_1")
	ck.Eq(0, node.MaxCost, "MaxCost")
	ck.StrContains(c.notice, "budget cleared for c_1", "notice")
}

func TestBudgetFormFailureResetsBusyAndSurfacesError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "b")
	ck.Require().NotNil(c.budgetForm, "budgetForm is nil")
	f := c.budgetForm
	f.input.SetValue("10")
	// Submit
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.Require().NotNil(cmd, "cmd is nil on enter")
	ck.Require().True(f.busy, "f.busy should be true after submit")

	// Simulate RPC failure
	rpcErr := errors.New("rpc error: code = InvalidArgument desc = budget exceeds parent")
	c.applyBudgetSet(budgetSetMsg{origin: f, childID: "c_1", name: "c_1", maxCost: 10, err: rpcErr})

	ck.Require().NotNil(c.budgetForm, "modal should still be open after failure")
	ck.False(f.busy, "busy should be false after failure so user can retry")
	ck.Eq("code = InvalidArgument desc = budget exceeds parent", f.err, "f.err")
	ck.StrContains(c.notice, "code = InvalidArgument desc = budget exceeds parent", "notice")

	// Enter can be pressed again to dispatch a new command
	_, cmd2 := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.NotNil(cmd2, "enter should dispatch new command on retry")
	ck.True(f.busy, "busy should be true after second submit")
}

func TestBudgetFormCrossModalCorrelation(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	c.selected = "c_1"
	press(c, "b")
	formA := c.budgetForm
	formA.input.SetValue("25")
	_, cmdA := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.Require().NotNil(cmdA, "cmdA is nil")

	// Operator cancels form A (esc clears c.budgetForm) and switches to child c_2 modal while request A is in flight
	press(c, "esc")
	ck.Require().Nil(c.budgetForm, "budgetForm should be nil after esc")
	c.selected = "c_2"
	press(c, "b")
	formB := c.budgetForm
	ck.Require().False(formB == nil || formB == formA, "formB should be a new form for c_2")

	// Now deliver A's delayed success message
	c.applyBudgetSet(budgetSetMsg{origin: formA, childID: "c_1", name: "c_1", maxCost: 25.0, err: nil})

	// Form B should STILL be open
	ck.Eq(formB, c.budgetForm, "c.budgetForm was dismissed or replaced, want formB to stay open")

	// Rail for c_1 should be updated to 25.0
	nodeA, ok := c.rail.Get("c_1")
	ck.False(!ok || nodeA.MaxCost != 25.0, "c_1 MaxCost = %v, want 25.0", nodeA.MaxCost)

	// Deliver A's delayed failure to test failure path with different active modal
	formA.busy = true
	c.applyBudgetSet(budgetSetMsg{origin: formA, childID: "c_1", name: "c_1", maxCost: 25.0, err: errors.New("fail")})
	// Form B must not receive form A's error
	ck.Eq("", formB.err, "formB received formA's error")
}

func TestBudgetFormPlaceholderRenderWidth(t *testing.T) {
	form := newBudgetForm("c_1", "worker", 0, nil)
	rendered := form.view(80, 24, nil)
	stripped := ansi.Strip(rendered)
	assert.NewCollecting(t).StrContains(stripped, "(unlimited)", "rendered view missing full placeholder '(unlimited)':\n")
}

// ── resume ───────────────────────────────────────────────────────────────────

func TestResumeKeyOnlyActsOnAnExitedRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")

	ck.Nil(press(c, "r"), "r on a live row issued a command")
	ck.StrContains(c.notice, "not exited", "notice")

	exitChild(c, "c_1")
	ck.NotNil(press(c, "r"), "r on an exited row issued no resume")
	ck.StrContains(c.notice, "resuming", "notice")
}

func TestResumeFailureIsReportedAndSuccessReseeds(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")

	c.Update(resumedMsg{childID: "c_1", name: "one", err: errors.New("boom")})
	ck.StrContains(c.notice, "could not resume", "notice")
	ck.False(c.reseedInFlight, "a failed resume must not reseed")

	c.Update(resumedMsg{childID: "c_1", name: "one"})
	ck.StrContains(c.notice, "resumed one", "notice")
	ck.True(c.reseedInFlight, "a resume must reseed")
}

// ── send to an exited agent ──────────────────────────────────────────────────

func exitedSendFailure(childID, text string) sendFailedMsg {
	err := rpcreason.Attach(connect.NewError(connect.CodeFailedPrecondition, errors.New("child has exited")), protocol.ErrChildExited)
	return sendFailedMsg{err: err, childID: childID, mode: rafikiv1.SendMode_SEND_MODE_PROMPT, text: text}
}

// A send to an exited agent used to vanish into a status line. It must instead
// ask, and keep what was typed until the question is answered.
func TestSendToExitedAgentAsksToResume(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	exitChild(c, "c_1")
	msg := exitedSendFailure("c_1", "hello")
	msg.attempt = c.optimistic.add("c_1", ackText, "hello", nil)

	c.Update(msg)

	ck.Require().NotNil(c.resumeAsk, "an exited-agent send failure must open the resume question")
	ck.Eq("hello", c.resumeAsk.text, "draft held by the question")
	ck.EqDeep([]string(nil), pendingTexts(c), "the pending marker must clear")
	ck.StrContains(c.notice, "resume", "notice")
}

func TestSendFailureForOtherReasonsStaysAStatusLine(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")

	c.Update(sendFailedMsg{err: errors.New("boom"), childID: "c_1", text: "hello"})

	ck.Nil(c.resumeAsk, "only child_exited may open the resume question")
	ck.StrContains(c.status, "send failed", "status")
}

func TestAbortToExitedAgentDoesNotAskToResume(t *testing.T) {
	c := railWith(t, "c_1")
	msg := exitedSendFailure("c_1", "")
	msg.mode = rafikiv1.SendMode_SEND_MODE_ABORT

	c.Update(msg)

	assert.NewCollecting(t).Nil(c.resumeAsk, "aborting an exited agent must not offer to resume it")
}

func TestResumeAskYesResumesAndSendsAfterwards(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	exitChild(c, "c_1")
	c.Update(exitedSendFailure("c_1", "hello"))

	ck.NotNil(press(c, "y"), "y must issue the resume")
	ck.Nil(c.resumeAsk, "answering must close the question")

	cmd := c.applyResumed(resumedMsg{childID: "c_1", name: "c_1", resend: &resumeAsk{
		childID: "c_1", name: "c_1", mode: rafikiv1.SendMode_SEND_MODE_PROMPT, text: "hello",
	}})
	ck.NotNil(cmd, "a successful resume must carry the held message on")
	ck.EqDeep([]string{"hello"}, pendingTexts(c), "the re-send shows the message as pending")
}

func TestResumeAskAnyOtherKeyKeepsTheDraft(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	exitChild(c, "c_1")
	c.Update(exitedSendFailure("c_1", "hello"))

	ck.Nil(press(c, "n"), "declining must not issue a command")
	ck.Nil(c.resumeAsk, "answering must close the question")
	ck.Eq("hello", c.ta.Value(), "declined send must come back as the draft")
}

func TestDeclinedDraftIsPrependedToWhatWasTypedSince(t *testing.T) {
	c := railWith(t, "c_1")
	c.Update(exitedSendFailure("c_1", "hello"))
	c.ta.SetValue("next")

	press(c, "n")

	assert.NewCollecting(t).Eq("hello\nnext", c.ta.Value(), "draft must not overwrite newer input")
}

func TestFailedResumeGivesTheDraftBack(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")

	cmd := c.applyResumed(resumedMsg{childID: "c_1", name: "c_1", err: errors.New("boom"), resend: &resumeAsk{
		childID: "c_1", name: "c_1", text: "hello",
	}})

	ck.Nil(cmd, "a failed resume must not send")
	ck.Eq("hello", c.ta.Value(), "draft")
	ck.StrContains(c.notice, "could not resume", "notice")
}

// ── rename modal ─────────────────────────────────────────────────────────────

func TestRenameKeyOpensModalPrefilled(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha", Status: "idle", Labels: map[string]string{}},
		{ChildId: "c_2", Name: "beta", Status: "idle", Labels: map[string]string{}},
	})
	c.focus = focusRail
	c.selected = "c_1"

	press(c, "R")

	ck.Require().NotNil(c.renameForm, "renameForm is nil after pressing 'R' on rail")
	ck.Eq("c_1", c.renameForm.childID, "childID")
	ck.Eq("alpha", c.renameForm.input.Value(), "prefilled name")
}

func TestRenameFormRejectsBlankName(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.selected = "c_1"
	press(c, "R")
	ck.Require().NotNil(c.renameForm, "renameForm is nil")

	c.renameForm.input.SetValue("   ")
	_, cmd := c.handleKey(tea.KeyPressMsg{Code: 13, Text: "enter"})
	ck.Nil(cmd, "cmd returned for a blank name, want nil")
	ck.NotEq("", c.renameForm.err, "expected an error for a blank name")
	ck.False(c.renameForm.busy, "busy is true on validation error")
}

func TestAppliedRenameUpdatesRailAndDismissesModal(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "alpha", Status: "idle", Labels: map[string]string{}}})
	c.selected = "c_1"
	press(c, "R")
	f := c.renameForm
	ck.Require().NotNil(f, "renameForm is nil")

	c.applyRenamed(renamedMsg{origin: f, childID: "c_1", name: "gamma"})

	n, _ := c.rail.Get("c_1")
	ck.Eq("gamma", n.Name, "rail name")
	ck.Nil(c.renameForm, "modal still up after a successful rename")
}

// ── home executor and spawn warnings ─────────────────────────────────────────

// executorRowFor is a ListExecutors row: Machine mirrors Labels["machine"], as
// the daemon builds it.
func executorRowFor(machine string, eligible bool, labels map[string]string) *rafikiv1.ExecutorRow {
	l := map[string]string{"machine": machine}
	for k, v := range labels {
		l[k] = v
	}
	return &rafikiv1.ExecutorRow{Id: "e_" + machine, Machine: machine, Labels: l, Eligible: eligible}
}

// The form's home rule, resolved where Update can do it safely (sealHome) and
// carried on the params — never inside a tea.Cmd closure, where buildSpawnRequest
// runs and where the executor cache would be read concurrently with Update's
// write. It names a machine only when the daemon already listed it eligible AND
// inside a selector the CALLER declared: the daemon refuses a home outside the
// child's reachable set, so a stale guess would turn a spawn into a refusal.
func TestHomeExecutorSealedInUpdateFromAnEligibleRow(t *testing.T) {
	ck := assert.NewAborting(t)
	t.Setenv(paths.ExecutorName, "greyshift")
	c := &Cockpit{
		executorSelector:         "owner=brent",
		executorSelectorFromFlag: true,
		executors: map[string][]*rafikiv1.ExecutorRow{
			protocol.KindFundi: {
				executorRowFor("silvershift", true, map[string]string{"owner": "brent"}),
				executorRowFor("greyshift", true, map[string]string{"owner": "brent"}),
			},
		},
	}

	req := c.buildSpawnRequest(c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"}))
	ck.Eq("greyshift", req.GetHomeExecutor(), "this machine's eligible row must become the home")
	ck.Eq("owner=brent", req.GetExecutorSelector(), "the declared selector still rides along")

	// An ineligible row, a row outside the selector, and a not-yet-fetched
	// answer all leave home empty — "the daemon resolves a home".
	c.executors[protocol.KindFundi] = []*rafikiv1.ExecutorRow{
		executorRowFor("greyshift", false, map[string]string{"owner": "brent"}),
	}
	ck.Eq("", c.buildSpawnRequest(c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"})).GetHomeExecutor(),
		"an ineligible machine must not be named")

	c.executors[protocol.KindFundi] = []*rafikiv1.ExecutorRow{
		executorRowFor("greyshift", true, map[string]string{"env": "home"}),
	}
	c.executorSelector = "env=prod"
	ck.Eq("", c.buildSpawnRequest(c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"})).GetHomeExecutor(),
		"a machine outside the selector must not be named")

	c.executors = map[string][]*rafikiv1.ExecutorRow{}
	c.executorSelector = "owner=brent"
	ck.Eq("", c.buildSpawnRequest(c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"})).GetHomeExecutor(),
		"an answer that has not arrived is not a match")
}

// The request builder runs INSIDE the tea.Cmd closures, off Update, and the
// executor cache is written by Update. It must therefore never consult that
// cache — even with an eligible local row cached and a declared selector, an
// unsealed params yields no home. Resolving it there is the data race this
// split exists to prevent.
func TestBuildSpawnRequestIgnoresCachedExecutorRows(t *testing.T) {
	ck := assert.NewAborting(t)
	t.Setenv(paths.ExecutorName, "greyshift")
	c := &Cockpit{
		executorSelector:         "owner=brent",
		executorSelectorFromFlag: true,
		executors: map[string][]*rafikiv1.ExecutorRow{
			protocol.KindFundi: {executorRowFor("greyshift", true, map[string]string{"owner": "brent"})},
		},
	}

	ck.Eq("", c.buildSpawnRequest(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"}).GetHomeExecutor(),
		"buildSpawnRequest must not read the executor cache; sealHome resolves home in Update")

	p := c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"})
	ck.Eq("greyshift", p.home, "sealHome is where the cached row is read")
	ck.Eq("greyshift", c.buildSpawnRequest(p).GetHomeExecutor(), "the sealed home reaches the wire")
}

// A session executor's selector already names exactly this machine, so the
// daemon resolves the same home from it and the field would only add a name to
// get wrong. An explicit executor field likewise leaves home to the daemon.
func TestHomeExecutorLeftToTheDaemonForASessionExecutor(t *testing.T) {
	ck := assert.NewAborting(t)
	t.Setenv(paths.ExecutorName, "greyshift")
	c := &Cockpit{
		executorSelector: "owner=brent",
		executors: map[string][]*rafikiv1.ExecutorRow{
			protocol.KindFundi: {executorRowFor("greyshift", true, map[string]string{"owner": "brent"})},
		},
	}

	ck.Eq("", c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"}).home,
		"a session executor needs no explicit home")

	p := c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/tmp", executor: "greyshift"})
	req := c.buildSpawnRequest(p)
	ck.Eq("greyshift", req.GetExecutorRef(), "the form's executor field")
	ck.Eq("", req.GetHomeExecutor(), "a named executor leaves home to the daemon")
}

// switchHomeClient answers the source read and records the spawn, so a switch's
// request and warnings can be inspected without a daemon.
type switchHomeClient struct {
	rafikiv1connect.ControlClient

	labels   map[string]string
	warnings []string
	spawned  *rafikiv1.SpawnRequest
}

func (f *switchHomeClient) GetChild(_ context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
		ChildId: req.Msg.GetChildId(),
		Name:    "scout",
		Cwd:     "/work/proj",
		Labels:  f.labels,
	}}), nil
}

// GetHistory answers the focus command the spawn's hop produces — running
// applySpawned's batch executes it — with an empty conversation. The switch it
// is used for never reads it.
func (f *switchHomeClient) GetHistory(
	context.Context, *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{}), nil
}

func (f *switchHomeClient) Spawn(_ context.Context, req *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	f.spawned = req.Msg
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: "c_new", Warnings: f.warnings}), nil
}

func (f *switchHomeClient) Kill(context.Context, *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	return connect.NewResponse(&rafikiv1.KillResponse{}), nil
}

func (f *switchHomeClient) Close(context.Context, *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	return connect.NewResponse(&rafikiv1.CloseResponse{}), nil
}

func (f *switchHomeClient) Rename(_ context.Context, req *connect.Request[rafikiv1.RenameRequest]) (*connect.Response[rafikiv1.RenameResponse], error) {
	return connect.NewResponse(&rafikiv1.RenameResponse{Name: req.Msg.GetName()}), nil
}

func (f *switchHomeClient) SetRouting(_ context.Context, req *connect.Request[rafikiv1.SetRoutingRequest]) (*connect.Response[rafikiv1.SetRoutingResponse], error) {
	return connect.NewResponse(&rafikiv1.SetRoutingResponse{ChildId: req.Msg.GetChildId()}), nil
}

// A switch continues the same work on the same machine, so its fork carries the
// source's home. It is set UNCONDITIONALLY: an unlabelled source clears any home
// the form's own rule would have inferred, because the CLI switch leaves that
// one to the daemon and the two faces must agree.
func TestHomeExecutorCarriedFromTheSwitchSource(t *testing.T) {
	ck := assert.NewAborting(t)
	t.Setenv(paths.ExecutorName, "greyshift")
	client := &switchHomeClient{labels: map[string]string{"rafiki/home-executor": "silvershift"}}
	// A form rule that WOULD name this machine, so the unlabelled case below
	// proves the switch override rather than an empty cache.
	c := &Cockpit{
		client:                   client,
		executorSelector:         "owner=brent",
		executorSelectorFromFlag: true,
		executors: map[string][]*rafikiv1.ExecutorRow{
			protocol.KindFundi: {executorRowFor("greyshift", true, map[string]string{"owner": "brent"})},
		},
	}
	sealed := c.sealHome(spawnParams{kind: protocol.KindFundi, cwd: "/work/proj", switchSource: "c_src"})
	ck.Eq("greyshift", sealed.home, "the form's own rule would name this machine")

	req, _, err := c.switchRequest(context.Background(), sealed)
	ck.Require().NoError(err, "switchRequest")
	ck.Eq("silvershift", req.GetHomeExecutor(), "the source's home must ride the fork")

	client.labels = map[string]string{}
	req, _, err = c.switchRequest(context.Background(), sealed)
	ck.Require().NoError(err, "switchRequest")
	ck.Eq("", req.GetHomeExecutor(),
		"an unlabelled source must clear the inferred home, exactly as the CLI switch sends none")
}

// The daemon's warnings must survive the switch: applySpawned's notice is
// replaced by applySwitched's as soon as the close+rename lands, so a warning
// only on the first one is a warning nobody reads.
//
// Driven through the Cmd applySpawned actually returns — tea.BatchMsg is an
// exported []tea.Cmd, so the batch can be taken apart without running the
// program — which exercises the argument applySpawned hands the finish step,
// not just the finish step's own behaviour.
func TestSwitchWarningsSurviveTheFinishNotice(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.client = &switchHomeClient{}
	c.form.busy = true

	batched := c.applySpawned(spawnedMsg{
		childID:      "c_new",
		switchSource: "c_src",
		sourceName:   "scout",
		warnings:     []string{"home executor not connected"},
	})
	ck.StrContains(c.notice, "warning: home executor not connected", "the spawn notice names it")

	batch, ok := batched().(tea.BatchMsg)
	ck.Require().True(ok, "applySpawned answered %T, want a tea.BatchMsg", batched())
	var sw switchedMsg
	found := false
	for _, cmd := range batch {
		if cmd == nil {
			continue
		}
		if msg, ok := cmd().(switchedMsg); ok {
			sw, found = msg, true
		}
	}
	ck.Require().True(found, "the batch carried no switch finish step")
	ck.Require().NoError(sw.err, "switch tail")
	c.applySwitched(sw)

	ck.StrContains(c.notice, "switched", "the finish notice is the one that stays")
	ck.StrContains(c.notice, "warning: home executor not connected",
		"the finish notice must still name the spawn warning")
}

// The warnings reach spawnedMsg from the Spawn response on BOTH spawn paths;
// deleting either copy leaves the operator with nothing to read.
func TestSpawnCmdCarriesSpawnWarnings(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Cockpit{client: &switchHomeClient{warnings: []string{"selector matches no live executor"}}}

	msg, ok := c.spawnCmd(spawnParams{kind: protocol.KindFundi, cwd: "/tmp"})().(spawnedMsg)
	ck.Require().True(ok, "spawnCmd answered %T", msg)
	ck.Require().NoError(msg.err, "spawnCmd")
	ck.EqDeep([]string{"selector matches no live executor"}, msg.warnings, "spawnCmd warnings")
}

func TestSwitchSpawnCmdCarriesSpawnWarnings(t *testing.T) {
	ck := assert.NewAborting(t)
	client := &switchHomeClient{
		labels:   map[string]string{"rafiki/home-executor": "silvershift"},
		warnings: []string{"home executor not connected"},
	}
	c := &Cockpit{client: client}

	msg, ok := c.switchSpawnCmd(spawnParams{
		kind: protocol.KindFundi, cwd: "/work/proj", switchSource: "c_src",
	})().(spawnedMsg)
	ck.Require().True(ok, "switchSpawnCmd answered %T", msg)
	ck.Require().NoError(msg.err, "switchSpawnCmd")
	ck.EqDeep([]string{"home executor not connected"}, msg.warnings, "switchSpawnCmd warnings")
	ck.Eq("silvershift", client.spawned.GetHomeExecutor(), "the switch's fork carried the source's home")
}

// The daemon's warnings are non-fatal: the child spawned and is usable, so
// they belong on the status line rather than in the form's error field — one
// line, the first warning plus a count of the rest.
func TestSpawnWarningsShownInStatus(t *testing.T) {
	t.Run("one warning", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		c := formCockpit(t)
		c.applySpawned(spawnedMsg{childID: "c_new", warnings: []string{"no home executor resolved"}})

		ck.Nil(c.form, "a warning must not keep the form open")
		ck.StrContains(c.notice, "warning: no home executor resolved", "notice")
		ck.False(strings.Contains(c.notice, "more"), "a single warning needs no count: %q", c.notice)
	})

	t.Run("several warnings", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		c := formCockpit(t)
		c.applySpawned(spawnedMsg{childID: "c_new", warnings: []string{
			"selector matches no live executor",
			"no home executor resolved",
			"home executor not connected",
		}})

		ck.StrContains(c.notice, "created c_new", "the created id still names the child")
		ck.StrContains(c.notice, "warning: selector matches no live executor", "notice")
		ck.StrContains(c.notice, "(+2 more)", "the remaining warnings collapse into a count")
	})

	t.Run("no warnings", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		c := formCockpit(t)
		c.applySpawned(spawnedMsg{childID: "c_new"})

		ck.Eq("created c_new", c.notice, "a clean spawn says only what it made")
	})
}
