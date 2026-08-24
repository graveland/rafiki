package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
)

// seedChild inserts a minimal childstore record directly, bypassing a real
// spawn -- these tests exercise label plumbing, not the spawn path.
func seedChild(t *testing.T, c *Controller) string {
	t.Helper()
	childID := "c_" + t.Name()
	c.st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    "idle",
		Cwd:       "/tmp",
		StartedAt: time.Now(),
		Labels:    map[string]string{},
	})
	return childID
}

// A child that binds AFTER spawn -- because no executor matched yet, or because
// its first one died -- must end up with rafiki/executor and rafiki/workspace
// pointing at where it actually is. Everything downstream reads the childstore:
// handleChildExit releases the workspace by these labels, and HandleExecutorLost
// finds affected children by them. A stale pair leaks the live workspace and
// releases a dead one.
func TestNoteBindingUpdatesAnExistingChildsLabels(t *testing.T) {
	c := newTestController(t)
	childID := seedChild(t, c) // whatever this package already uses to insert a session

	b := &controllerBinder{c: c}
	b.NoteBinding(childID, "exec-A", "ws-1")
	b.NoteBinding(childID, "exec-B", "ws-2")

	snap, ok := c.st.Get(childID)
	if !ok {
		t.Fatal("child vanished")
	}
	if snap.Labels["rafiki/executor"] != "exec-B" {
		t.Fatalf(`rafiki/executor = %q, want exec-B -- a rebind that does not `+
			`reach the childstore leaks the new workspace and releases the old`,
			snap.Labels["rafiki/executor"])
	}
	if snap.Labels["rafiki/workspace"] != "ws-2" {
		t.Fatalf(`rafiki/workspace = %q, want ws-2`, snap.Labels["rafiki/workspace"])
	}
}

// Before the child record exists, NoteBinding has nowhere to write, so the
// spawn-time stash is still needed -- but only as a bridge, and Spawn must
// consume it under the mutex.
func TestNoteBindingBeforeTheChildExistsIsPickedUpAtSpawn(t *testing.T) {
	c := newTestController(t)
	b := &controllerBinder{c: c}
	b.NoteBinding("c_notyet", "exec-A", "ws-1")

	wl, ok := c.takeWorkspaceLabels("c_notyet")
	if !ok {
		t.Fatal("the pre-spawn stash must survive until Spawn consumes it")
	}
	if wl.executorID != "exec-A" || wl.workspaceID != "ws-1" {
		t.Fatalf("got %+v", wl)
	}
	if _, again := c.takeWorkspaceLabels("c_notyet"); again {
		t.Fatal("taking must delete: the map grew without bound because the " +
			"only delete was the spawn-time one")
	}
}

// Reproduces the crash: Spawn reads and deletes c.wsLabels with no lock while a
// tool goroutine writes it under wsLabelsMu. Concurrent map read+write is a
// fatal error -- unrecoverable, and it takes the daemon with it.
func TestWorkspaceLabelsAreRaceFree(t *testing.T) {
	c := newTestController(t)
	var wg sync.WaitGroup
	b := &controllerBinder{c: c}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.NoteBinding("c_race", "exec-A", "ws-1")
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.takeWorkspaceLabels("c_race")
		}()
	}
	wg.Wait()
	_ = context.Background()
}
