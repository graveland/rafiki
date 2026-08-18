package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestComputeLineageLabels(t *testing.T) {
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "top", Labels: map[string]string{}})
	st.Insert(&childstore.Session{ChildID: "mid", Labels: map[string]string{
		childstore.LabelParent: "top",
		childstore.LabelRoot:   "top",
	}})

	t.Run("no parent means no labels", func(t *testing.T) {
		parent, root, err := computeLineageLabels(st, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parent != "" || root != "" {
			t.Fatalf("got parent=%q root=%q; want both empty", parent, root)
		}
	})

	t.Run("child of a top-level parent roots at that parent", func(t *testing.T) {
		parent, root, err := computeLineageLabels(st, "top")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parent != "top" || root != "top" {
			t.Fatalf("got parent=%q root=%q; want top,top", parent, root)
		}
	})

	t.Run("grandchild inherits the parent's root", func(t *testing.T) {
		parent, root, err := computeLineageLabels(st, "mid")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parent != "mid" || root != "top" {
			t.Fatalf("got parent=%q root=%q; want mid,top", parent, root)
		}
	})

	t.Run("unknown parent is rejected", func(t *testing.T) {
		_, _, err := computeLineageLabels(st, "ghost")
		if err == nil {
			t.Fatal("expected an error for an unknown parent, got nil")
		}
	})
}

func TestSpawnStampsLineageLabels(t *testing.T) {
	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, t.Context(), nil, nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	// Spawn a top-level child.
	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Type: protocol.TypeCtrlSpawn,
		Cwd:  os.TempDir(),
		Kind: protocol.KindPi,
	})
	if err != nil {
		t.Fatalf("spawn top-level: %v", err)
	}
	parentID := res.ChildID

	// Verify top-level child has no lineage labels.
	top, ok := st.Get(parentID)
	if !ok {
		t.Fatalf("top-level child not found in store")
	}
	if _, exists := top.Labels[childstore.LabelParent]; exists {
		t.Error("top-level child should not have rafiki/parent label")
	}
	if _, exists := top.Labels[childstore.LabelRoot]; exists {
		t.Error("top-level child should not have rafiki/root label")
	}

	// Kill the top-level child so we can spawn a second one (in-process pi
	// children don't stack — wait for the first to exit).
	ch, ok := ctrl.cm.Get(parentID)
	if !ok {
		t.Fatalf("could not find child %q in manager", parentID)
	}
	_, _ = ch.Shutdown(5*time.Second, 1*time.Second)
	if !waitForChildRemoval(ctrl.cm, parentID, 5*time.Second) {
		t.Fatalf("child %q not removed from manager after shutdown", parentID)
	}

	// Spawn a child with ParentChildID set.
	res2, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Cwd:           os.TempDir(),
		Kind:          protocol.KindPi,
		ParentChildID: parentID,
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	childID := res2.ChildID

	snap, ok := st.Get(childID)
	if !ok {
		t.Fatalf("child %q not found in store after spawn", childID)
	}
	if got := snap.Labels[childstore.LabelParent]; got != parentID {
		t.Errorf("rafiki/parent = %q, want %q", got, parentID)
	}
	if got := snap.Labels[childstore.LabelRoot]; got != parentID {
		t.Errorf("rafiki/root = %q, want %q", got, parentID)
	}
}

func TestSpawnRejectsUnknownParent(t *testing.T) {
	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, t.Context(), nil, nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	before := len(st.List())

	_, err = ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Cwd:           os.TempDir(),
		ParentChildID: "c_does_not_exist",
	})
	if err == nil {
		t.Fatal("expected an error for unknown parent, got nil")
	}

	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *control.ControllerError, got %T: %v", err, err)
	}
	if ce.Code != protocol.ErrChildNotFound {
		t.Errorf("error code = %q, want %q", ce.Code, protocol.ErrChildNotFound)
	}

	// No new child should have appeared in the store.
	if after := len(st.List()); after != before {
		t.Fatalf("store grew from %d to %d children — rejection must happen before spawn", before, after)
	}
}

func TestResumePreservesLineageLabels(t *testing.T) {
	ctrl := newTestController(t)

	// 1. Spawn a top-level child A.
	parentID := spawnTestChild(t, ctrl, nil)

	// Verify A has no lineage labels.
	snapA, ok := ctrl.st.Get(parentID)
	if !ok {
		t.Fatalf("child A not found")
	}
	if _, exists := snapA.Labels[childstore.LabelParent]; exists {
		t.Error("top-level child A should not have rafiki/parent label")
	}

	// 2. Spawn child B with ParentChildID = A.
	req := protocol.SpawnRequest{
		Cwd:           t.TempDir(),
		PiBinary:      fakePiBin(t),
		NoSession:     true,
		ParentChildID: parentID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("spawn child B: %v", err)
	}
	childBID := res.ChildID

	// Verify B got the lineage labels.
	snapB, ok := ctrl.st.Get(childBID)
	if !ok {
		t.Fatalf("child B not found")
	}
	if got := snapB.Labels[childstore.LabelParent]; got != parentID {
		t.Fatalf("B.LabelParent = %q, want %q", got, parentID)
	}
	if got := snapB.Labels[childstore.LabelRoot]; got != parentID {
		t.Fatalf("B.LabelRoot = %q, want %q", got, parentID)
	}

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
	if !ok {
		t.Fatalf("child B not found after resume")
	}
	if got := snapResumed.Labels[childstore.LabelParent]; got != parentID {
		t.Fatalf("after resume B.LabelParent = %q, want %q — resume dropped the parent label", got, parentID)
	}
	if got := snapResumed.Labels[childstore.LabelRoot]; got != parentID {
		t.Fatalf("after resume B.LabelRoot = %q, want %q — resume dropped the root label", got, parentID)
	}
}

// tree.go reads fundi/parent as an authoritative fallback for pre-rename
// records. If a client can WRITE it, lineage is forgeable and IsDescendant —
// the authority predicate for every steering verb — returns a false positive
// across agents.
func TestReservedLabelPrefixesRejectBothSpellings(t *testing.T) {
	for _, key := range []string{
		"rafiki/parent", "rafiki/root", "fundi/parent", "fundi/root", "fundi/anything",
	} {
		if err := validateUserLabelKeys(map[string]string{key: "c_victim"}); err == nil {
			t.Errorf("validateUserLabelKeys accepted %q; lineage must not be settable by a caller", key)
		}
		if err := validateUserRemoveKeys([]string{key}); err == nil {
			t.Errorf("validateUserRemoveKeys accepted %q", key)
		}
	}
	// An ordinary label must still work.
	if err := validateUserLabelKeys(map[string]string{"team": "infra"}); err != nil {
		t.Errorf("ordinary label rejected: %v", err)
	}
}
