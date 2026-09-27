package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

func TestComputeLineageLabels(t *testing.T) {
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "top", Labels: map[string]string{}})
	st.Insert(&childstore.Session{ChildID: "mid", Labels: map[string]string{
		childstore.LabelParent: "top",
		childstore.LabelRoot:   "top",
	}})

	t.Run("no parent means no labels", func(t *testing.T) {
		c := assert.NewAborting(t)
		parent, root, err := computeLineageLabels(st, "")
		c.NoError(err, "unexpected error")
		c.False(parent != "" || root != "", "got parent=%q root=%q; want both empty", parent, root)
	})

	t.Run("child of a top-level parent roots at that parent", func(t *testing.T) {
		c := assert.NewAborting(t)
		parent, root, err := computeLineageLabels(st, "top")
		c.NoError(err, "unexpected error")
		c.False(parent != "top" || root != "top", "got parent=%q root=%q; want top,top", parent, root)
	})

	t.Run("grandchild inherits the parent's root", func(t *testing.T) {
		c := assert.NewAborting(t)
		parent, root, err := computeLineageLabels(st, "mid")
		c.NoError(err, "unexpected error")
		c.False(parent != "mid" || root != "top", "got parent=%q root=%q; want mid,top", parent, root)
	})

	t.Run("unknown parent is rejected", func(t *testing.T) {
		_, _, err := computeLineageLabels(st, "ghost")
		assert.NewAborting(t).Error(err, "expected an error for an unknown parent, got nil")
	})
}

func TestSpawnStampsLineageLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, false, t.Context(), nil, nil, nil, nil)

	// Spawn a top-level child.
	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Cwd:      os.TempDir(),
		Kind:     protocol.KindClaude,
		PiBinary: fakePiBin(t),
	}, users.Identity{})
	c.Require().NoError(err, "spawn top-level")
	parentID := res.ChildID

	// Verify top-level child has no lineage labels.
	top, ok := st.Get(parentID)
	c.Require().True(ok, "top-level child not found in store")
	if _, exists := top.Labels[childstore.LabelParent]; exists {
		t.Error("top-level child should not have rafiki/parent label")
	}
	_, exists := top.Labels[childstore.LabelRoot]
	c.False(exists, "top-level child should not have rafiki/root label")

	// Kill the top-level child so we can spawn a second one (in-process pi
	// children don't stack — wait for the first to exit).
	ch, ok := ctrl.cm.Get(parentID)
	c.Require().True(ok, "could not find child %q in manager", parentID)
	_, _ = ch.Shutdown(5*time.Second, 1*time.Second)
	c.Require().True(waitForChildRemoval(ctrl.cm, parentID, 5*time.Second), "child %q not removed from manager after shutdown", parentID)

	// Spawn a child with ParentChildID set.
	res2, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Cwd:           os.TempDir(),
		Kind:          protocol.KindClaude,
		PiBinary:      fakePiBin(t),
		ParentChildID: parentID,
	}, users.Identity{})
	c.Require().NoError(err, "spawn child")
	childID := res2.ChildID

	snap, ok := st.Get(childID)
	c.Require().True(ok, "child %q not found in store after spawn", childID)
	c.Eq(parentID, snap.Labels[childstore.LabelParent], "rafiki/parent")
	c.Eq(parentID, snap.Labels[childstore.LabelRoot], "rafiki/root")
}

func TestSpawnRejectsUnknownParent(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, false, t.Context(), nil, nil, nil, nil)

	before := len(st.List())

	_, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Cwd:           os.TempDir(),
		ParentChildID: "c_does_not_exist",
	}, users.Identity{})
	c.Require().Error(err, "expected an error for unknown parent, got nil")

	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "expected *connectapi.ControllerError, got %T: %v", err, err)
	c.Eq(protocol.ErrChildNotFound, ce.Code, "error code")

	// No new child should have appeared in the store.
	c.Require().Eq(before, len(st.List()), "store grew from")
}

func TestResumePreservesLineageLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	ctrl := newTestController(t)

	// 1. Spawn a top-level child A.
	parentID := spawnTestChild(t, ctrl, nil)

	// Verify A has no lineage labels.
	snapA, ok := ctrl.st.Get(parentID)
	c.Require().True(ok, "child A not found")
	_, exists := snapA.Labels[childstore.LabelParent]
	c.False(exists, "top-level child A should not have rafiki/parent label")

	// 2. Spawn child B with ParentChildID = A.
	req := protocol.SpawnRequest{
		Kind:          protocol.KindClaude,
		Cwd:           t.TempDir(),
		PiBinary:      fakePiBin(t),
		NoSession:     true,
		ParentChildID: parentID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	c.Require().NoError(err, "spawn child B")
	childBID := res.ChildID

	// Verify B got the lineage labels.
	snapB, ok := ctrl.st.Get(childBID)
	c.Require().True(ok, "child B not found")
	c.Require().Eq(parentID, snapB.Labels[childstore.LabelParent], "B.LabelParent")
	c.Require().Eq(parentID, snapB.Labels[childstore.LabelRoot], "B.LabelRoot")

	// 3. Kill B and wait for removal.
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if _, err := ctrl.Kill(killCtx, childBID, 2000, 500); err != nil {
		t.Fatalf("kill B: %v", err)
	}
	waitForExited(t, ctrl.st, childBID, 5*time.Second)

	// 4. Resume B.
	resCtx, resCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resCancel()
	if _, err := ctrl.Resume(resCtx, childBID, ""); err != nil {
		t.Fatalf("resume B: %v", err)
	}

	// 5. Assert B's labels STILL contain lineage.
	snapResumed, ok := ctrl.st.Get(childBID)
	c.Require().True(ok, "child B not found after resume")
	c.Require().Eq(parentID, snapResumed.Labels[childstore.LabelParent], "after resume B.LabelParent")
	c.Require().Eq(parentID, snapResumed.Labels[childstore.LabelRoot], "after resume B.LabelRoot")
}

// tree.go reads fundi/parent as an authoritative fallback for pre-rename
// records. If a client can WRITE it, lineage is forgeable and IsDescendant —
// the authority predicate for every steering verb — returns a false positive
// across agents.
func TestReservedLabelPrefixesRejectBothSpellings(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, key := range []string{
		"rafiki/parent", "rafiki/root", "fundi/parent", "fundi/root", "fundi/anything", "owner",
	} {
		c.Error(validateUserLabelKeys(map[string]string{key: "c_victim"}), "validateUserLabelKeys accepted %q; lineage must not be settable by a caller", key)
		c.Error(validateUserRemoveKeys([]string{key}), "validateUserRemoveKeys accepted %q", key)
	}
	// An ordinary label must still work.
	c.NoError(validateUserLabelKeys(map[string]string{"team": "infra"}), "ordinary label rejected")
}
