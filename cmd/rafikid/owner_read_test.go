// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ownerReadChild is one row of the fixture: a child id, its direct parent ("" for
// a top-level child), the owner recorded on its OWN row ("" when it inherits),
// and when it started.
type ownerReadChild struct {
	id      string
	parent  string
	owner   string
	started time.Time
}

// ownerReadFixture builds a controller whose childstore holds exactly these
// children. Parent/root labels are written the way a real spawn writes them, so
// the lineage walk (IsDescendant, OwnerUserIDForChild) sees a real chain.
func ownerReadFixture(t *testing.T, children ...ownerReadChild) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	roots := map[string]string{}
	for _, ch := range children {
		labels := map[string]string{}
		root := ch.id
		if ch.parent != "" {
			labels[childstore.LabelParent] = ch.parent
			root = roots[ch.parent]
			if root == "" {
				root = ch.parent
			}
			labels[childstore.LabelRoot] = root
		}
		roots[ch.id] = root
		c.st.Insert(&childstore.Session{
			ChildID:     ch.id,
			Status:      protocol.StatusIdle,
			Labels:      labels,
			StartedAt:   ch.started,
			Kind:        protocol.KindFundi,
			OwnerUserID: ch.owner,
		})
	}
	return c
}

// ownerReadBase is the shared shape: two same-owner top-level trees (A and B),
// a foreign owner's tree (C), and an ownerless tree (D). A1 and B1 inherit their
// owner through the parent chain rather than carrying it themselves.
func ownerReadBase(t *testing.T) *Controller {
	t.Helper()
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	return ownerReadFixture(t,
		ownerReadChild{id: "c_a", owner: "u1", started: t0},
		ownerReadChild{id: "c_a1", parent: "c_a", started: t0.Add(1 * time.Minute)},
		ownerReadChild{id: "c_b", owner: "u1", started: t0.Add(2 * time.Minute)},
		ownerReadChild{id: "c_b1", parent: "c_b", started: t0.Add(3 * time.Minute)},
		ownerReadChild{id: "c_c", owner: "u2", started: t0.Add(4 * time.Minute)},
		ownerReadChild{id: "c_d", started: t0.Add(5 * time.Minute)},
		ownerReadChild{id: "c_d1", parent: "c_d", started: t0.Add(6 * time.Minute)},
	)
}

// A target beneath the caller is readable — the pre-existing subtree rule still
// holds, so this change only ever adds reads.
func TestCanReadChildAdmitsDescendant(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.True(c.canReadChild("c_a", "c_a1"), "a caller must read its own descendant")
}

// A same-owner sibling in ANOTHER top-level tree is readable: that is the whole
// point of the change, and it is the case the subtree rule refuses.
func TestCanReadChildAdmitsSameOwnerInAnotherTree(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.True(c.canReadChild("c_a", "c_b"), "same-owner target in another tree must be readable")
	ck.True(c.canReadChild("c_a", "c_b1"), "same-owner target beneath another tree must be readable")
}

// A foreign owner's target is refused.
func TestCanReadChildRefusesForeignOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("c_a", "c_c"), "a foreign owner's child must not be readable")
	ck.False(c.canReadChild("c_c", "c_a"), "and the refusal must be symmetric")
}

// An unknown target is refused — and it must be indistinguishable from a
// foreign owner's target, which is why canReadChild answers a bool and the
// refusal text is written once by the caller.
func TestCanReadChildRefusesUnknownTarget(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("c_a", "c_nope"), "an unknown target must not be readable")
}

// A caller with no owner anywhere in its lineage keeps subtree-only reads: it
// can read its own descendants and nothing else.
func TestCanReadChildRefusesOwnerlessCaller(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("c_d", "c_a"), "an ownerless caller must not read outside its subtree")
	ck.False(c.canReadChild("c_d", "c_c"), "an ownerless caller must not read a foreign child")
	ck.True(c.canReadChild("c_d", "c_d1"), "an ownerless caller still reads its own subtree")
}

// A target with no owner anywhere in its lineage is refused to an ownerful
// caller: an unowned lineage is admin/UDS-only, exactly as for recall.
func TestCanReadChildRefusesTargetWithoutOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("c_a", "c_d"), "a target with no owner must not be readable")
	ck.False(c.canReadChild("c_a", "c_d1"), "nor anything beneath an unowned target")
}

// Empty ids are refused before any lookup.
func TestCanReadChildRefusesEmptyCaller(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("", "c_a"), "an empty caller must read nothing")
}

func TestCanReadChildRefusesEmptyTarget(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)
	ck.False(c.canReadChild("c_a", ""), "an empty target must not be readable")
}

// ownedSnapshots splits the caller's readable set into its subtree and the rest
// of its owner's children: subtree first in Descendants order, others
// newest-first with ties by child id, the caller excluded from both, and the
// foreign owner's and unowned children in neither.
func TestCanReadChildOwnedSnapshots(t *testing.T) {
	ck := assert.NewCollecting(t)
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c := ownerReadFixture(t,
		ownerReadChild{id: "c_a", owner: "u1", started: t0},
		ownerReadChild{id: "c_a1", parent: "c_a", started: t0.Add(1 * time.Minute)},
		ownerReadChild{id: "c_a2", parent: "c_a", started: t0.Add(2 * time.Minute)},
		ownerReadChild{id: "c_b", owner: "u1", started: t0.Add(4 * time.Minute)},
		ownerReadChild{id: "c_e", owner: "u1", started: t0.Add(4 * time.Minute)},
		ownerReadChild{id: "c_c", owner: "u2", started: t0.Add(5 * time.Minute)},
		ownerReadChild{id: "c_d", started: t0.Add(6 * time.Minute)},
	)

	subtree, others, ok := c.ownedSnapshots("c_a")
	ck.Require().True(ok, "an ownerful caller with a live row resolves")

	subtreeIDs := make([]string, 0, len(subtree))
	for _, snap := range subtree {
		subtreeIDs = append(subtreeIDs, snap.ChildID)
	}
	ck.EqDeep([]string{"c_a2", "c_a1"}, subtreeIDs, "subtree is the caller's descendants, newest first")

	otherIDs := make([]string, 0, len(others))
	for _, snap := range others {
		otherIDs = append(otherIDs, snap.ChildID)
	}
	// c_b and c_e tie on StartedAt; the id breaks the tie. c_c (foreign) and c_d
	// (no owner) are absent, and the caller c_a is never listed.
	ck.EqDeep([]string{"c_b", "c_e"}, otherIDs, "others is the owner's other children, newest first, ties by id")

	ck.NotContains(subtreeIDs, "c_a", "the caller is not in its own subtree list")
	ck.NotContains(otherIDs, "c_a", "the caller is not in its own others list")
	ck.NotContains(otherIDs, "c_c", "a foreign owner's child is not in the owner list")
	ck.NotContains(otherIDs, "c_d", "an unowned child is not in the owner list")
}

// An ownerless caller has no owner scope at all: ok is false with nil slices, so
// its caller falls back to the subtree list alone.
func TestCanReadChildOwnedSnapshotsOwnerlessCaller(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := ownerReadBase(t)

	subtree, others, ok := c.ownedSnapshots("c_d")
	ck.False(ok, "an ownerless caller resolves no owner scope")
	ck.Empty(subtree, "no subtree on an ownerless answer")
	ck.Empty(others, "no others on an ownerless answer")

	_, _, ok = c.ownedSnapshots("c_nope")
	ck.False(ok, "an unknown caller resolves no owner scope")

	_, _, ok = c.ownedSnapshots("")
	ck.False(ok, "an empty caller resolves no owner scope")
}
