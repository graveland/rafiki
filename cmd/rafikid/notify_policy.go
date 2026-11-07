// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// wakesSpawner reports whether a TURN-END settle of a child should reach its
// spawner. A child on the default policy always wakes it. A quiet child
// (protocol.NotifyResult) wakes it only when the turn left something the spawner
// must act on: a recorded result, a pending question, a turn error, or the child's
// own budget cap being breached. Exit and failure settles do not go through this.
func wakesSpawner(snap childstore.Snapshot, turnFailed, ownCapBreached bool) bool {
	if snap.NotifyPolicy != protocol.NotifyResult {
		return true
	}
	return snap.Result != "" || snap.PendingQuestion != "" || snap.LastTurnError != "" || turnFailed || ownCapBreached
}

// uncleanPending reports whether childID's stored turn outcome is unclean,
// without consuming it: settleReason takes it afterwards.
func (s *turnOutcomeStore) uncleanPending(childID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[childID]
	return ok && !o.Clean
}

// notifyTurnEnd is the turn-end settle: the same fan-out as notifySubagentSettled,
// dropped for a quiet child that left nothing for its spawner. checkTaskResidue
// still runs on a dropped settle (it nudges the child itself).
func (c *Controller) notifyTurnEnd(childID string) {
	snap, ok := c.st.Get(childID)
	if ok && !wakesSpawner(snap, c.turnOutcomes.uncleanPending(childID), c.breaches.has(childID)) {
		c.turnOutcomes.take(childID)
		c.checkTaskResidue(childID)
		return
	}
	c.notifySubagentSettled(childID, c.settleReason(childID), "", "")
}
