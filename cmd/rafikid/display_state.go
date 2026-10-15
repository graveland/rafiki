package main

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// displayFacts is the one derived-value input set for a child's operator
// display state. Every field is either the child's own status or a fact about
// outstanding work; computeDisplayState folds them into a single
// (DisplayState, WaitingOn) pair.
type displayFacts struct {
	Status          protocol.Status
	TopLevel        bool // no parent
	PendingQuestion string
	LastTurnError   string
	BudgetHalted    bool
	PendingEvents   bool // news queued in the event buffer, not yet delivered
	OpenSchedules   int
	RunningJobs     int
	LiveDescendants int
}

// computeDisplayState folds a child's facts into one display value. The rules
// are applied IN ORDER; the first match returns:
//
//  1. exited: the process is gone.
//  2. not idle: every other status means the child is doing something. This
//     covers every working status, spawning, shutting_down, and any
//     unrecognised or empty status; the test is "not idle and not exited",
//     not a membership check, so a new status never silently reads as idle.
//  3. idle: the child is settled, and what (if anything) it waits on is
//     decided by precedence operator > failed > children > events > job >
//     schedule > idle.
//
// It always returns a non-empty DisplayState; DisplayState("") is never
// returned. WaitingOn is non-empty exactly when the state is DisplayWaiting.
func computeDisplayState(f displayFacts) (protocol.DisplayState, protocol.WaitingOn) {
	if f.Status == protocol.StatusExited {
		return protocol.DisplayExited, ""
	}
	if f.Status != protocol.StatusIdle {
		return protocol.DisplayWorking, ""
	}
	if f.TopLevel && (f.PendingQuestion != "" || f.BudgetHalted) {
		return protocol.DisplayWaiting, protocol.WaitingOnOperator
	}
	if f.LastTurnError != "" {
		return protocol.DisplayFailed, ""
	}
	if f.LiveDescendants > 0 {
		return protocol.DisplayWaiting, protocol.WaitingOnChildren
	}
	if f.PendingEvents {
		return protocol.DisplayWaiting, protocol.WaitingOnEvents
	}
	if f.RunningJobs > 0 {
		return protocol.DisplayWaiting, protocol.WaitingOnJob
	}
	if f.OpenSchedules > 0 {
		return protocol.DisplayWaiting, protocol.WaitingOnSchedule
	}
	return protocol.DisplayIdle, ""
}

// displayMessage is the human-facing one-liner for a computed display state,
// or "" when there is nothing useful to say (idle, working, exited).
//
// The operator message is the child's own pending question when it has one,
// and only otherwise the budget-halt sentence; a failure message is the last
// turn's error. Both question and error are single lines: the first line is
// taken, trimmed, and truncated to 200 runes.
func displayMessage(f displayFacts, ds protocol.DisplayState, on protocol.WaitingOn) string {
	switch ds {
	case protocol.DisplayWaiting:
		if on == protocol.WaitingOnOperator {
			if f.PendingQuestion != "" {
				return firstLineTruncated(f.PendingQuestion)
			}
			if f.BudgetHalted {
				return "over budget: raise the cap to continue"
			}
		}
	case protocol.DisplayFailed:
		if f.LastTurnError != "" {
			return firstLineTruncated(f.LastTurnError)
		}
	}
	return ""
}

// firstLineTruncated returns s up to its first newline, trimmed of surrounding
// whitespace and truncated to 200 runes.
func firstLineTruncated(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	const max = 200
	if runes := []rune(s); len(runes) > max {
		return string(runes[:max])
	}
	return s
}
