// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// A spawn whose INITIAL child row cannot be persisted must be REFUSED and fully
// unwound: the lineage walk cannot cross a missing row, so a child that ran
// without one would hide its whole sub-subtree from its ancestor's spend. The
// error is returned, the child is in neither the store nor the process map, and
// no process is left launched.
//
// Fails against the pre-change spawn, which logged the failed insert and
// carried on.
func TestSpawnRefusedWhenInitialRowCannotBePersisted(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	fs := &failingChildStore{}
	ctrl.children = fs

	_, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       os.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().Error(err, "a spawn whose initial row cannot be written must be refused")

	ck.Require().Eq(1, len(fs.failed), "exactly the initial insert is attempted; got %v", fs.failed)
	childID := fs.failed[0]
	ck.StrContains(err.Error(), childID, "the refusal must name the child id: %v", err)

	if _, ok := ctrl.st.Get(childID); ok {
		t.Fatalf("the refused spawn left %s in the in-memory store", childID)
	}
	if _, ok := ctrl.cm.Get(childID); ok {
		t.Fatalf("the refused spawn left %s in the process map, so a process was left launched", childID)
	}
}

// The other half of the guard: a SUCCESSFUL persist must spawn exactly as
// before, so the refusal above is not over-broad.
func TestSpawnProceedsWhenInitialRowPersists(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.children = stubChildStore{}

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       os.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "a spawn whose initial row persists must succeed")

	if _, ok := ctrl.st.Get(res.ChildID); !ok {
		t.Fatalf("the spawned child %s is absent from the store", res.ChildID)
	}
	if _, ok := ctrl.cm.Get(res.ChildID); !ok {
		t.Fatalf("the spawned child %s is absent from the process map", res.ChildID)
	}
}
