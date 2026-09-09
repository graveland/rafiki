// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/server"
)

// A remote cockpit's spawn must be attributed to the caller the proxy face
// authenticated. The owner is matched by executor admission selectors, so an
// unowned child is not merely untidy.
func TestSpawnOwnerComesFromTheAuthenticatedFace(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
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

// S1: Connect's agent-control verbs refuse a credential that is not a user
// credential. The child-attributed identity carries the owner's UserID — the
// attribution path that bills the child's LLM turns — so a non-empty-UserID
// check cannot stand in for provenance here either. Both adapters hold a nil
// Controller: reaching it at all means the refusal failed.
func TestConnectAgentControlRefusesANonUserCredential(t *testing.T) {
	childAttributed := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed})
	unknown := server.WithIdentity(context.Background(), &server.Identity{})
	const refusal = "agent-control verbs require a user credential"

	_, err := (connectLifecycle{}).Spawn(childAttributed, connectapi.SpawnParams{})
	if err == nil || !strings.Contains(err.Error(), refusal+"; this identity is child-attributed") {
		t.Fatalf("child-attributed spawn = %v, want the named refusal", err)
	}
	_, err = (connectLifecycle{}).Spawn(unknown, connectapi.SpawnParams{})
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("unknown-credential spawn = %v, want the named refusal", err)
	}
	_, err = (connectExecutors{}).ListExecutors(childAttributed, "")
	if err == nil || !strings.Contains(err.Error(), refusal+"; this identity is child-attributed") {
		t.Fatalf("child-attributed ListExecutors = %v, want the named refusal", err)
	}
}

// The gate itself: nil identity (the UDS path, where the socket is the
// credential and spawns land unowned) and a real user credential both pass.
func TestRequireUserCredentialAdmitsAnonymousAndUser(t *testing.T) {
	if err := requireUserCredential(context.Background()); err != nil {
		t.Fatalf("anonymous UDS caller refused: %v", err)
	}
	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
	if err := requireUserCredential(user); err != nil {
		t.Fatalf("user credential refused: %v", err)
	}
}
