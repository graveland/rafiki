// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestOwnerUserIDForChild(t *testing.T) {
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

	uid, ok := ctrl.OwnerUserIDForChild(res.ChildID)
	if !ok || uid != "u_owner1" {
		t.Fatalf("OwnerUserIDForChild = (%q, %v), want (%q, true)", uid, ok, "u_owner1")
	}
}

func TestOwnerUserIDForChildUnknownChild(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)

	if _, ok := ctrl.OwnerUserIDForChild("c_does_not_exist"); ok {
		t.Fatal("want ok=false for an unknown child")
	}
}
