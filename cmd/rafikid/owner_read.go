// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sort"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// notReadableText is the ONE refusal a caller gets for a target it may not
// read: a foreign owner, an unknown id and a lineage with no owner all produce
// this same sentence, so a caller cannot learn which of the three it hit. It is
// a format string taking the target id.
//
// Reads and acts are deliberately different sentences: acting on an agent is
// still bounded by the spawn tree, reading is bounded by the owner.
const notReadableText = "agent %s is not one you can read; you may read agents your owner owns, and steer only agents you spawned (or that they spawned). Use agent_list with scope=\"owner\" to see what you can read"

// canReadChild reports whether callerChildID may READ target: target is in the
// caller's subtree, or target and the caller resolve (OwnerUserIDForChild) to the
// same NON-EMPTY owner. Anything unresolvable — empty ids, an unknown caller or
// target, a lineage with no owner — is false. Acting on a child never uses this;
// it stays on st.IsDescendant.
func (c *Controller) canReadChild(callerChildID, target string) bool {
	if callerChildID == "" || target == "" {
		return false
	}
	if c.st.IsDescendant(callerChildID, target) {
		return true
	}
	callerOwner, ok := c.OwnerUserIDForChild(callerChildID)
	if !ok || callerOwner == "" {
		return false
	}
	targetOwner, ok := c.OwnerUserIDForChild(target)
	if !ok || targetOwner == "" {
		return false
	}
	return callerOwner == targetOwner
}

// ownedSnapshots splits the live children a caller may READ by owner scope.
//
// ok is false — with nil slices — when the caller's owner does not resolve to a
// non-empty id, or the caller has no live row: an ownerless caller has no owner
// scope at all, and its caller falls back to the subtree list alone.
//
// Otherwise subtree is the caller's own descendants (Descendants' order), and
// others is every other live child whose owner (OwnerUserIDForChild) equals the
// caller's, excluding the caller itself and anything already in subtree. others
// is newest-first, ties broken by child id, so a list is stable across calls.
func (c *Controller) ownedSnapshots(callerChildID string) (subtree, others []childstore.Snapshot, ok bool) {
	if callerChildID == "" {
		return nil, nil, false
	}
	owner, ok := c.OwnerUserIDForChild(callerChildID)
	if !ok || owner == "" {
		return nil, nil, false
	}
	if _, live := c.st.Get(callerChildID); !live {
		return nil, nil, false
	}
	subtree = c.st.Descendants(callerChildID)
	inSubtree := make(map[string]struct{}, len(subtree))
	for _, snap := range subtree {
		inSubtree[snap.ChildID] = struct{}{}
	}
	// Enumerate live snapshots exactly as ListChildren does, so this list and
	// the operator list never disagree about what "live" means.
	for _, snap := range c.List(protocol.ListFilter{}) {
		if snap.ChildID == callerChildID {
			continue
		}
		if _, dup := inSubtree[snap.ChildID]; dup {
			continue
		}
		snapOwner, ok := c.OwnerUserIDForChild(snap.ChildID)
		if !ok || snapOwner == "" || snapOwner != owner {
			continue
		}
		others = append(others, snap)
	}
	sort.Slice(others, func(i, j int) bool {
		if !others[i].StartedAt.Equal(others[j].StartedAt) {
			return others[i].StartedAt.After(others[j].StartedAt)
		}
		return others[i].ChildID < others[j].ChildID
	})
	return subtree, others, true
}
