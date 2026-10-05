// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

func TestSpawnOwnerSurvivesResume(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{UserID: "u_owner1"})
	c.NoError(err, "spawn")
	childID := res.ChildID

	snap, ok := ctrl.st.Get(childID)
	c.False(!ok || snap.OwnerUserID != "u_owner1", "fresh spawn OwnerUserID = %q, ok=%v, want %q, true", snap.OwnerUserID, ok, "u_owner1")

	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if _, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	resCtx, resCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resCancel()
	if _, err := ctrl.Resume(resCtx, childID, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	resumedSnap, ok := ctrl.st.Get(childID)
	c.False(!ok || resumedSnap.OwnerUserID != "u_owner1", "after resume OwnerUserID = %q, ok=%v, want %q, true — resume dropped the owner", resumedSnap.OwnerUserID, ok, "u_owner1")
}
