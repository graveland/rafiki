// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"log/slog"
	"strings"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// maxClaudeTurnErrorBytes caps a claude turn failure before it lands on the row
// and in the settle: the proxy's reason can carry up to 8 KiB of upstream body.
const maxClaudeTurnErrorBytes = 1024

// noteClaudeTurnFailure records that a main-thread request of a claude child
// failed upstream or mid-stream, so the child's turn-end settle and display
// state see a failed turn the way a fundi child's OnTurnEnded makes them. The
// failure stands until a later request of the same turn succeeds
// (clearClaudeTurnFailure) or the next turn starts (handleStatusChange).
//
// The capture report that carries a failure is sent after the request's last
// write, so Claude Code's result frame can win the race and idle the child
// first. A quiet child's settle was then dropped for lack of a reason, so it is
// sent here; a default child already got its "settled (idle)".
func (c *Controller) noteClaudeTurnFailure(childID, text string) {
	snap, ok := c.st.Get(childID)
	if !ok || snap.Kind != protocol.KindClaude || snap.Native {
		return
	}
	if len(text) > maxClaudeTurnErrorBytes {
		text = strings.ToValidUTF8(text[:maxClaudeTurnErrorBytes], "") + "…(truncated)"
	}
	if err := c.st.Update(childID, func(s *childstore.Session) { s.LastTurnError = text }); err != nil {
		slog.Warn("record claude turn error", "childId", childID, "error", err)
		return
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("persist claude turn error", "childId", childID, "error", err)
	}
	c.turnOutcomes.set(childID, fundi.TurnOutcome{Err: errors.New(text)})

	snap, ok = c.st.Get(childID)
	if !ok || snap.Status != protocol.StatusIdle {
		return
	}
	// Already idle: the idle transition has run, so nothing will take the
	// outcome just stored.
	o, _ := c.turnOutcomes.take(childID)
	if snap.NotifyPolicy == protocol.NotifyResult && c.evbuf != nil {
		c.notifySubagentSettled(childID, "settled after a turn error: "+turnErrorText(o), "", "")
	}
}

// clearClaudeTurnFailure drops a recorded failure when a later main-thread
// request of the same child succeeded: Claude Code retried and the model
// answered, so the turn did not fail.
func (c *Controller) clearClaudeTurnFailure(childID string) {
	snap, ok := c.st.Get(childID)
	if !ok || snap.Kind != protocol.KindClaude || snap.Native || snap.LastTurnError == "" {
		return
	}
	if err := c.st.Update(childID, func(s *childstore.Session) { s.LastTurnError = "" }); err != nil {
		slog.Warn("clear claude turn error", "childId", childID, "error", err)
		return
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("persist cleared claude turn error", "childId", childID, "error", err)
	}
	c.turnOutcomes.take(childID)
}
