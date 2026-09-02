// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
