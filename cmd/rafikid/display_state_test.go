package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestComputeDisplayStateExited: an exited child reads as exited even when it
// has an outstanding operator question — the process is gone, nothing can
// answer it.
func TestComputeDisplayStateExited(t *testing.T) {
	a := assert.NewCollecting(t)
	f := displayFacts{
		Status:          protocol.StatusExited,
		TopLevel:        true,
		PendingQuestion: "should I proceed?",
		BudgetHalted:    true,
	}
	ds, on := computeDisplayState(f)
	a.Eq(protocol.DisplayExited, ds, "exited wins over a pending question")
	a.Eq(protocol.WaitingOn(""), on, "exited is not waiting")
}

// TestComputeDisplayStateNotIdleIsWorking: every status that is not exited and
// not idle reads as working, regardless of any waiting-shaped facts. The rule
// is "not idle and not exited", so an empty or unrecognised status is working
// too, never idle.
func TestComputeDisplayStateNotIdleIsWorking(t *testing.T) {
	statuses := []protocol.Status{
		protocol.StatusStreaming,
		protocol.StatusToolRunning,
		protocol.StatusCompacting,
		protocol.StatusBatchWait,
		protocol.StatusRunning,
		protocol.StatusBlockedUI,
		protocol.StatusSpawning,
		protocol.StatusShuttingDown,
		"",
		"bogus",
	}
	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			a := assert.NewCollecting(t)
			f := displayFacts{
				Status:          st,
				TopLevel:        true,
				PendingQuestion: "are you there?",
				BudgetHalted:    true,
				LastTurnError:   "boom",
				OpenSchedules:   1,
				RunningJobs:     1,
				LiveDescendants: 1,
				PendingEvents:   true,
			}
			ds, on := computeDisplayState(f)
			a.Eq(protocol.DisplayWorking, ds, "not-idle status is working")
			a.Eq(protocol.WaitingOn(""), on, "working is not waiting")
		})
	}
}

// TestComputeDisplayStateOperatorNeedsTopLevel: a pending question on a child
// that has a parent is NOT the operator's to answer. It must not read as
// operator; with nothing else set it falls through to idle.
func TestComputeDisplayStateOperatorNeedsTopLevel(t *testing.T) {
	a := assert.NewCollecting(t)
	f := displayFacts{
		Status:          protocol.StatusIdle,
		TopLevel:        false,
		PendingQuestion: "which approach?",
	}
	ds, on := computeDisplayState(f)
	a.NotEq(protocol.DisplayWaiting, ds, "a non-top-level question is not operator-waiting")
	a.Eq(protocol.DisplayIdle, ds, "falls through to idle")
	a.Eq(protocol.WaitingOn(""), on, "idle is not waiting")
}

// TestComputeDisplayStateBudgetHaltTopLevelOnly: a budget halt is the
// operator's only for a top-level child; a spawned child's halt is its
// parent's problem.
func TestComputeDisplayStateBudgetHaltTopLevelOnly(t *testing.T) {
	a := assert.NewCollecting(t)

	top := displayFacts{Status: protocol.StatusIdle, TopLevel: true, BudgetHalted: true}
	ds, on := computeDisplayState(top)
	a.Eq(protocol.DisplayWaiting, ds, "top-level budget halt waits")
	a.Eq(protocol.WaitingOnOperator, on, "on the operator")

	child := displayFacts{Status: protocol.StatusIdle, TopLevel: false, BudgetHalted: true}
	ds, on = computeDisplayState(child)
	a.NotEq(protocol.DisplayWaiting, ds, "a child's budget halt is not operator-waiting")
	a.Eq(protocol.DisplayIdle, ds, "falls through to idle")
	a.Eq(protocol.WaitingOn(""), on, "idle is not waiting")
}

// TestComputeDisplayStatePrecedence: the idle-state rules are ordered
// operator > failed > children > events > job > schedule > idle. For each
// adjacent pair, setting both facts must yield the higher-priority one.
func TestComputeDisplayStatePrecedence(t *testing.T) {
	pairs := []struct {
		name string
		// lower and higher each set one fact on the shared zero fact set.
		lower  func(f *displayFacts)
		higher func(f *displayFacts)
		wantD  protocol.DisplayState
		wantO  protocol.WaitingOn
	}{
		{
			name:   "operator over failed",
			lower:  func(f *displayFacts) { f.LastTurnError = "boom" },
			higher: func(f *displayFacts) { f.TopLevel, f.PendingQuestion = true, "proceed?" },
			wantD:  protocol.DisplayWaiting,
			wantO:  protocol.WaitingOnOperator,
		},
		{
			name:   "failed over children",
			lower:  func(f *displayFacts) { f.LiveDescendants = 2 },
			higher: func(f *displayFacts) { f.LastTurnError = "boom" },
			wantD:  protocol.DisplayFailed,
			wantO:  "",
		},
		{
			name:   "children over events",
			lower:  func(f *displayFacts) { f.PendingEvents = true },
			higher: func(f *displayFacts) { f.LiveDescendants = 2 },
			wantD:  protocol.DisplayWaiting,
			wantO:  protocol.WaitingOnChildren,
		},
		{
			name:   "events over job",
			lower:  func(f *displayFacts) { f.RunningJobs = 3 },
			higher: func(f *displayFacts) { f.PendingEvents = true },
			wantD:  protocol.DisplayWaiting,
			wantO:  protocol.WaitingOnEvents,
		},
		{
			name:   "job over schedule",
			lower:  func(f *displayFacts) { f.OpenSchedules = 1 },
			higher: func(f *displayFacts) { f.RunningJobs = 3 },
			wantD:  protocol.DisplayWaiting,
			wantO:  protocol.WaitingOnJob,
		},
		{
			name:   "schedule over idle",
			lower:  func(f *displayFacts) {},
			higher: func(f *displayFacts) { f.OpenSchedules = 1 },
			wantD:  protocol.DisplayWaiting,
			wantO:  protocol.WaitingOnSchedule,
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			a := assert.NewCollecting(t)
			f := displayFacts{Status: protocol.StatusIdle}
			p.lower(&f)
			p.higher(&f)
			ds, on := computeDisplayState(f)
			a.Eq(p.wantD, ds, "higher-priority fact wins")
			a.Eq(p.wantO, on, "waiting-on matches the winner")
		})
	}
}

// TestComputeDisplayStatePendingEventsOnlyWhenIdle: pending events never make
// a working child read as waiting, and a lone idle child with pending events
// waits on events.
func TestComputeDisplayStatePendingEventsOnlyWhenIdle(t *testing.T) {
	a := assert.NewCollecting(t)

	working := displayFacts{Status: protocol.StatusStreaming, PendingEvents: true}
	ds, on := computeDisplayState(working)
	a.Eq(protocol.DisplayWorking, ds, "pending events do not unseat working")
	a.Eq(protocol.WaitingOn(""), on, "working is not waiting")

	idle := displayFacts{Status: protocol.StatusIdle, PendingEvents: true}
	ds, on = computeDisplayState(idle)
	a.Eq(protocol.DisplayWaiting, ds, "idle with pending events waits")
	a.Eq(protocol.WaitingOnEvents, on, "on events")
}

// TestComputeDisplayStateIdle: a settled child with no outstanding facts is
// idle, waiting on nothing.
func TestComputeDisplayStateIdle(t *testing.T) {
	a := assert.NewCollecting(t)
	ds, on := computeDisplayState(displayFacts{Status: protocol.StatusIdle})
	a.Eq(protocol.DisplayIdle, ds, "all-zero facts are idle")
	a.Eq(protocol.WaitingOn(""), on, "idle is not waiting")
}

// TestComputeDisplayStateNeverEmpty: the zero value DisplayState("") is
// invalid and must never be returned, for any status and any facts.
func TestComputeDisplayStateNeverEmpty(t *testing.T) {
	statuses := []protocol.Status{
		protocol.StatusSpawning,
		protocol.StatusIdle,
		protocol.StatusStreaming,
		protocol.StatusRunning,
		protocol.StatusToolRunning,
		protocol.StatusCompacting,
		protocol.StatusBatchWait,
		protocol.StatusBlockedUI,
		protocol.StatusShuttingDown,
		protocol.StatusExited,
		"",
		"bogus",
	}
	factSets := []func(f *displayFacts){
		func(f *displayFacts) {},
		func(f *displayFacts) { f.TopLevel, f.PendingQuestion = true, "q" },
		func(f *displayFacts) { f.LastTurnError = "e" },
		func(f *displayFacts) { f.BudgetHalted = true },
		func(f *displayFacts) {
			f.LiveDescendants, f.RunningJobs, f.OpenSchedules = 1, 1, 1
			f.PendingEvents = true
		},
	}
	a := assert.NewCollecting(t)
	for _, st := range statuses {
		for i, set := range factSets {
			f := displayFacts{Status: st}
			set(&f)
			ds, _ := computeDisplayState(f)
			a.NotEq(protocol.DisplayState(""), ds, "status %q facts %d must yield a state", st, i)
		}
	}
}

// TestDisplayMessage: the human-facing one-liner for a computed state — first
// line only, trimmed, truncated to 200 runes, with the budget sentence for a
// budget-only operator halt and "" when there is nothing to say.
func TestDisplayMessage(t *testing.T) {
	a := assert.NewCollecting(t)

	// First-line extraction: only the first line, trimmed.
	f := displayFacts{Status: protocol.StatusIdle, TopLevel: true, PendingQuestion: "  first line  \nsecond line"}
	ds, on := computeDisplayState(f)
	a.Eq("first line", displayMessage(f, ds, on), "first line, trimmed")

	// Truncation to 200 runes on a multi-byte string.
	long := strings.Repeat("é", 250)
	f = displayFacts{Status: protocol.StatusIdle, TopLevel: true, PendingQuestion: long}
	ds, on = computeDisplayState(f)
	got := displayMessage(f, ds, on)
	a.Eq(200, len([]rune(got)), "truncated to 200 runes")
	a.Eq(strings.Repeat("é", 200), got, "truncation keeps whole runes")

	// Budget-only operator halt: the fixed sentence.
	f = displayFacts{Status: protocol.StatusIdle, TopLevel: true, BudgetHalted: true}
	ds, on = computeDisplayState(f)
	a.Eq(protocol.WaitingOnOperator, on, "budget halt waits on the operator")
	a.Eq("over budget: raise the cap to continue", displayMessage(f, ds, on), "budget sentence")

	// Failed: the first line of the last turn's error.
	f = displayFacts{Status: protocol.StatusIdle, LastTurnError: "kaboom\nstack trace here"}
	ds, on = computeDisplayState(f)
	a.Eq(protocol.DisplayFailed, ds, "last turn error is failed")
	a.Eq("kaboom", displayMessage(f, ds, on), "first line of the error")

	// "" for idle and working.
	f = displayFacts{Status: protocol.StatusIdle}
	ds, on = computeDisplayState(f)
	a.Eq("", displayMessage(f, ds, on), "idle has no message")

	f = displayFacts{Status: protocol.StatusStreaming}
	ds, on = computeDisplayState(f)
	a.Eq("", displayMessage(f, ds, on), "working has no message")
}
