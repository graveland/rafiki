// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// schedAbandonAfter bounds how long shutdown waits for a running timer
// callback before abandoning it (and logging its key).
const schedAbandonAfter = 30 * time.Second

// now is the controller's clock; a test-constructed Controller with no clock
// reads the wall clock.
func (c *Controller) now() time.Time {
	if c.clk == nil {
		return time.Now()
	}
	return c.clk.Now()
}

// at registers fn under key; a no-op when the controller has no scheduler.
func (c *Controller) at(key string, deadline time.Time, fn func()) {
	c.sched.At(key, deadline, fn)
}

// cancelTimer cancels key; a no-op without a scheduler.
func (c *Controller) cancelTimer(key string) {
	c.sched.Cancel(key)
}

// graceKey is the scheduler key for childID's grace deadline. Re-arming the
// same child replaces the pending job, so an exit that is superseded by a
// later one never leaves a second timer behind.
func graceKey(childID string) string { return "grace:" + childID }

// heartbeatKey is the scheduler key for childID's next heartbeat check-in.
func heartbeatKey(childID string) string { return "heartbeat:" + childID }

// armGrace closes childID at exitedAt + graceWindow. The callback re-reads
// the child and does nothing if it is gone, no longer exited, or exited
// again later (its newer exit armed its own deadline under the same key):
// every deadline here is derived, so a stale one is a wasted wakeup, not a
// bug.
//
// The skipped window (graceWindow <= 0) is the operator's way to disable
// grace entirely, and a zero exitedAt is a row with no exit to measure from —
// neither gets a timer.
func (c *Controller) armGrace(childID string, exitedAt time.Time) {
	if c.graceWindow <= 0 || exitedAt.IsZero() {
		return
	}
	c.at(graceKey(childID), exitedAt.Add(c.graceWindow), func() {
		snap, ok := c.st.Get(childID)
		if !ok || snap.Status != protocol.StatusExited || snap.ExitedAt.IsZero() {
			return
		}
		if c.now().Before(snap.ExitedAt.Add(c.graceWindow)) {
			return
		}
		if err := c.Close(childID); err != nil {
			slog.Warn("grace: close expired child", "childId", childID, "error", err)
			return
		}
		slog.Info("grace window expired; closed child", "childId", childID)
	})
}
