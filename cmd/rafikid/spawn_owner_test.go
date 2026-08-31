// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/server"
)

// A remote cockpit's spawn must be attributed to the caller the proxy face
// authenticated. The owner is matched by executor admission selectors, so an
// unowned child is not merely untidy.
func TestSpawnOwnerComesFromTheAuthenticatedFace(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent"})
	got := spawnOwner(ctx)
	if got.UserID != "u1" || got.Username != "brent" {
		t.Fatalf("owner = %+v, want the authenticated caller", got)
	}
	if !got.IsUser() {
		t.Fatal("want IsUser: only a user identity is persisted as owner_user_id")
	}
}

// The unix socket authenticates nobody — the socket is the credential — so an
// absent identity must be the zero value rather than a panic.
func TestSpawnOwnerIsZeroOnTheUnixSocket(t *testing.T) {
	got := spawnOwner(context.Background())
	if got.IsUser() {
		t.Fatalf("owner = %+v, want the zero identity for an unauthenticated local call", got)
	}
}
