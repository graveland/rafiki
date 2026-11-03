// SPDX-License-Identifier: Apache-2.0

package main

import "sync"

// publishLocks serializes publication per child, so one child's durable events
// reach subscribers in ORDINAL order.
//
// Ordinal order is not the same thing as ordinal allocation. eventlogdb
// allocates under a per-child advisory lock, but the publish that follows it is
// a separate step, and a publisher descheduled between the two lets a LATER
// append publish first. Every cursor-based consumer then sees the earlier event
// at or below its cursor and drops it as already delivered — connectapi's
// delivery.live does exactly that, and so does the cockpit's session cursor. A
// dropped user_message is a message that never reaches the transcript and a
// pending ghost that never clears.
//
// Striped rather than a map keyed by child id: there is nothing to clean up
// when a child is forgotten, and two children sharing a stripe merely serialize
// their publishes. A zero value is ready to use, which is what keeps a
// `&Controller{}` fixture working.
type publishLocks [64]sync.Mutex

// lock takes childID's stripe and returns its unlock.
func (l *publishLocks) lock(childID string) func() {
	// FNV-1a, inline: the child id is short and this runs once per event.
	var h uint32 = 2166136261
	for i := 0; i < len(childID); i++ {
		h ^= uint32(childID[i])
		h *= 16777619
	}
	mu := &l[h%uint32(len(l))]
	mu.Lock()
	return mu.Unlock
}
