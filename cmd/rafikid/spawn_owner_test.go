// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

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
// credential. The gate is the policy interceptor on the route
// (cmd/rafikid/connect_policy.go); these pin its decisions at the procedure
// level: Spawn and ListExecutors are userOnly, and both refusals name the
// procedure so an operator can tell what was refused.
func TestControlPolicyRefusesANonUserCredentialOnAgentControl(t *testing.T) {
	childAttributed := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed})
	unknown := server.WithIdentity(context.Background(), &server.Identity{})

	for _, procedure := range []string{
		controlProcedurePrefix + "Spawn",
		controlProcedurePrefix + "ListExecutors",
	} {
		for name, ctx := range map[string]context.Context{
			"child-attributed": childAttributed,
			"zero identity":    unknown,
		} {
			err := authorizeControlProcedure(ctx, procedure)
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s with a %s credential = %v, want %v",
					procedure, name, err, connect.CodePermissionDenied)
			}
			if !strings.Contains(err.Error(), procedure) {
				t.Errorf("refusal %q does not name the procedure", err)
			}
		}
	}
}

// The gate itself: nil identity (the UDS path, where the socket is the
// credential and spawns land unowned) and a real user credential both pass —
// for every policy, including the anyCaller verbs a child credential may
// reach.
func TestControlPolicyAdmitsAnonymousAndUser(t *testing.T) {
	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
	for _, ctx := range []context.Context{context.Background(), user} {
		for _, procedure := range []string{
			controlProcedurePrefix + "Spawn",
			controlProcedurePrefix + "ListExecutors",
			controlProcedurePrefix + "ListModels",
		} {
			if err := authorizeControlProcedure(ctx, procedure); err != nil {
				t.Errorf("%s refused: %v", procedure, err)
			}
		}
	}
}
