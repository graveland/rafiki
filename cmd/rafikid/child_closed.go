// SPDX-License-Identifier: Apache-2.0

package main

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// maxClosedAncestors bounds the chain a child_closed event carries. The tree is
// shallow by construction (spawn depth is capped), and the bound exists only so
// a malformed parent label cannot spin the walk that builds it.
const maxClosedAncestors = 64

// publishChildClosed announces a child that is about to leave the store.
//
// It MUST run BEFORE the store delete, and at every close site (Close, the
// cascade's native children, CloseAllExited): the chain and labels it carries
// are read from the snapshot, and after the delete there is nothing left to
// read.
//
// Why the event carries its own lineage: every subscriber that is still drawing
// the row — a cockpit attached to an ANCESTOR in particular — resolves "is this
// child in my scope?" against the childstore, which is exactly what a close
// empties. pkg/eventlog's eventLineage is the other half of that contract.
//
// Durable, deliberately: the rail stream subscribes at that tier, so an
// ephemeral close would reach nobody. A close publishes nothing else — the
// child's log is not replayed to a fresh subscriber (see delivery.replay) — so
// this is the only live notice membership can shrink.
func (c *Controller) publishChildClosed(childID string) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildClosed{ChildClosed: &rafikiv1.ChildClosed{
			Ancestors: c.ancestorChain(childID),
			Labels:    copyLabels(snap.Labels),
		}},
	})
}

// ancestorChain lists childID's ancestors, parent first and root last. A
// top-level child has none.
func (c *Controller) ancestorChain(childID string) []string {
	var out []string
	cur := childID
	for i := 0; i < maxClosedAncestors; i++ {
		parent, ok := c.st.ParentOf(cur)
		if !ok || parent == "" {
			break
		}
		out = append(out, parent)
		cur = parent
	}
	return out
}
