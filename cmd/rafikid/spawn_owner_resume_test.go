// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestSpawnOwnerSurvivesResume(t *testing.T) {
	t.Parallel()
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
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	childID := res.ChildID

	snap, ok := ctrl.st.Get(childID)
	if !ok || snap.OwnerUserID != "u_owner1" {
		t.Fatalf("fresh spawn OwnerUserID = %q, ok=%v, want %q, true", snap.OwnerUserID, ok, "u_owner1")
	}

	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if _, err := ctrl.Kill(killCtx, childID, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	resCtx, resCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resCancel()
	if _, err := ctrl.Resume(resCtx, childID, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	resumedSnap, ok := ctrl.st.Get(childID)
	if !ok || resumedSnap.OwnerUserID != "u_owner1" {
		t.Fatalf("after resume OwnerUserID = %q, ok=%v, want %q, true — resume dropped the owner",
			resumedSnap.OwnerUserID, ok, "u_owner1")
	}
}
