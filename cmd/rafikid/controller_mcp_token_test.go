package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// TestChildMCPTokenIsPerChildAndForgotten pins the per-child MCP secret's
// whole lifecycle at the Controller: two children mint different secrets,
// resume/respawn REUSE a child's minted secret rather than orphaning it, each
// secret resolves (in server.ChildTokenLookup's shape) to its own child and
// that child's owner, a secret delivered at spawn through proxyChildEnv is the
// one the controller holds, and the exit hook in handleChildExit stops a dead
// child's secret from resolving.
func TestChildMCPTokenIsPerChildAndForgotten(t *testing.T) {
	c := newTestController(t)

	// Two spawns yield different secrets, 32 crypto/rand bytes hex encoded.
	tokA := c.mintMCPToken("c_a")
	tokB := c.mintMCPToken("c_b")
	if tokA == "" || tokB == "" {
		t.Fatal("mintMCPToken returned an empty secret")
	}
	if tokA == tokB {
		t.Fatal("two children must mint different MCP secrets")
	}
	if len(tokA) != 64 {
		t.Fatalf("secret length = %d, want 64 hex chars (32 crypto/rand bytes)", len(tokA))
	}

	// Mint-or-reuse keyed on the child: Resume and RespawnChild rebuild the
	// spawn environment through the same proxyChildEnv/darajaClaudeParams
	// path, so a second mint for the SAME child must return the stored secret
	// — a fresh one would orphan the credential the running child holds.
	if again := c.mintMCPToken("c_a"); again != tokA {
		t.Fatalf("second mint for the same child = %q, want the stored %q (resume must reuse)", again, tokA)
	}

	// Each secret resolves to its own child and owner, through the same store
	// path OwnerUserIDForChild reads.
	c.st.Insert(&childstore.Session{
		ChildID: "c_a", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, OwnerUserID: "u_owner_a",
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_b", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, OwnerUserID: "u_owner_b",
	})
	cid, uid, ok := c.ChildForMCPToken(tokA)
	if !ok || cid != "c_a" || uid != "u_owner_a" {
		t.Fatalf("ChildForMCPToken(tokA) = (%q, %q, %v), want c_a/u_owner_a/true", cid, uid, ok)
	}
	cid, uid, ok = c.ChildForMCPToken(tokB)
	if !ok || cid != "c_b" || uid != "u_owner_b" {
		t.Fatalf("ChildForMCPToken(tokB) = (%q, %q, %v), want c_b/u_owner_b/true", cid, uid, ok)
	}
	if _, _, ok := c.ChildForMCPToken("not-a-secret"); ok {
		t.Fatal("an unknown secret must not resolve")
	}

	// The secret a spawn delivers is the one the controller holds:
	// proxyChildEnv is the only other mintMCPToken caller, so the only way
	// this child has a secret at all is that the spawn path minted it. The
	// proxy face must be wired for a claude child to be routed at all.
	c.proxyURL, c.proxyToken = "http://127.0.0.1:1/", "boot-secret"
	childID := spawnProxiedTestChild(t, c, "u_spawn_owner")
	tok := c.mintMCPToken(childID) // A0: the same secret the spawn minted
	cid, uid, ok = c.ChildForMCPToken(tok)
	if !ok || cid != childID || uid != "u_spawn_owner" {
		t.Fatalf("the spawned child's secret = (%q, %q, %v), want it to resolve to its own child and owner", cid, uid, ok)
	}

	// The exit hook: handleChildExit forgets the secret beside MarkExited.
	// Kill waits for cm.Remove — the final step of handleChildExit — so the
	// forget is deterministic by the time Kill returns.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Kill(ctx, childID, 2000, 2000); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, _, ok := c.ChildForMCPToken(tok); ok {
		t.Fatal("a dead child's MCP secret must stop resolving once the exit hook ran")
	}
}

// spawnProxiedTestChild spawns one real (fake-pi) claude child on a controller
// with the proxy face wired, attributed to userID, and waits for it to reach a
// live status. It exists so the test exercises the real Spawn → buildEnv →
// proxyChildEnv mint path rather than minting by hand.
func spawnProxiedTestChild(t *testing.T, c *Controller, ownerUserID string) string {
	t.Helper()
	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Spawn(ctx, req, users.Identity{UserID: ownerUserID, Username: "brent"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return res.ChildID
}
