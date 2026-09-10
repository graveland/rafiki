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

// TestMCPTokenSweepDropsEntriesForDeadChildren pins the credential map's
// memory bound. Two leak shapes exist: a mint whose spawn failed before the
// child row existed (forgetMCPToken only runs from handleChildExit), and a
// child whose row was closed by a sibling daemon. Both resolve ok=false
// forever and are pure memory; the sweep drops them when the map grows one
// threshold past its last sweep, and keeps entries whose child is alive.
func TestMCPTokenSweepDropsEntriesForDeadChildren(t *testing.T) {
	c := newTestController(t)
	c.mcpSweepAt = 3 // small threshold: the three entries this test mints reach it

	tokA := c.mintMCPToken("c_sweep_a")
	c.mintMCPToken("c_sweep_b") // never registered: the leak shape
	tokC := c.mintMCPToken("c_sweep_c")

	// A and C get store rows; A exits; B has no row at all — the two leak
	// shapes the sweep exists for, plus the live control.
	for _, id := range []string{"c_sweep_a", "c_sweep_c"} {
		// OwnerUserID must be non-empty: ChildForMCPToken refuses a child with
		// no recorded owner, and the surviving entry must still resolve.
		c.st.Insert(&childstore.Session{ChildID: id, OwnerUserID: "u-sweep", Status: protocol.StatusIdle})
	}
	if _, ok := c.st.SetStatus("c_sweep_a", protocol.StatusExited); !ok {
		t.Fatal("child A has no store row to exit")
	}

	if got := c.mcpTokensByChild["c_sweep_a"]; got != tokA {
		t.Fatalf("A's entry missing before the sweep")
	}

	c.sweepMCPTokensIfDue()

	if _, ok := c.mcpTokensByChild["c_sweep_a"]; ok {
		t.Fatal("the sweep kept an entry whose child has exited")
	}
	if _, ok := c.mcpTokensByChild["c_sweep_b"]; ok {
		t.Fatal("the sweep kept an entry whose child never registered (the spawn-failure leak shape)")
	}
	if _, ok := c.mcpTokensByChild["c_sweep_c"]; !ok {
		t.Fatal("the sweep dropped an entry whose child is alive")
	}
	if _, _, ok := c.ChildForMCPToken(tokC); !ok {
		t.Fatal("the surviving entry no longer resolves")
	}
	// The sweep must also drop both halves of a dropped entry: the secret key
	// in mcpTokens is gone, not orphaned.
	if got := len(c.mcpTokens); got != 1 {
		t.Fatalf("mcpTokens holds %d entries after the sweep, want 1 (both indexes move together)", got)
	}
	// And the next sweep is amortized: it must not fire again until the map
	// grows one threshold past the post-sweep size.
	if c.mcpSweepAt != 1+mcpTokenSweepThreshold {
		t.Fatalf("post-sweep trigger = %d, want live-set(1) + threshold", c.mcpSweepAt)
	}
}
