// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// The owner USER id is stamped at spawn with the subtree's owner: a user
// credential's spawn carries it, and an agent's spawn — the controllerSpawner
// both the MCP face and fundi children use — hands its own row's id down, so
// a descendant's row carries it too. Without that inheritance an
// agent-spawned child's row was unowned, and both lookups that need the user
// id — the per-child MCP secret's ChildTokenLookup and the per-boot token's
// X-Rafiki-Session attribution — refused it: every agent-spawned descendant's
// MCP call answered 401 ("unknown token") even though the daemon had minted
// and delivered the secret, and its fundi conversation went unattributed.

// TestOwnerUserIDForChildWalksTheAgentLineage pins the parent-chain walk for
// the rows the stamp cannot reach: children spawned before the inheritance
// landed keep an empty id across resume (the stored row is preserved, never
// backfilled), so the walk finds the owner on the nearest ancestor.
func TestOwnerUserIDForChildWalksTheAgentLineage(t *testing.T) {
	t.Parallel()
	c := newTestController(t)

	// Legacy rows, shaped as the pre-inheritance spawner left them: the
	// top-level child carried the id, each agent-spawned descendant only the
	// display-only owner LABEL.
	c.st.Insert(&childstore.Session{
		ChildID: "c_top", Status: protocol.StatusIdle,
		OwnerUserID: "u_top",
		Labels:      map[string]string{"owner": "brent"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_mid", Status: protocol.StatusIdle,
		Labels: map[string]string{"owner": "brent", childstore.LabelParent: "c_top"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_leaf", Status: protocol.StatusIdle,
		Labels: map[string]string{"owner": "brent", childstore.LabelParent: "c_mid"},
	})

	for _, tc := range []struct {
		childID string
		wantUID string
		wantOK  bool
	}{
		{"c_top", "u_top", true},  // own row
		{"c_mid", "u_top", true},  // one hop up
		{"c_leaf", "u_top", true}, // two hops up
		{"c_unknown", "", false},  // not registered at all
	} {
		uid, ok := c.OwnerUserIDForChild(tc.childID)
		if ok != tc.wantOK || uid != tc.wantUID {
			t.Errorf("OwnerUserIDForChild(%s) = (%q, %v), want (%q, %v)",
				tc.childID, uid, ok, tc.wantUID, tc.wantOK)
		}
	}

	// A lineage with NO owner anywhere — the anonymous local-socket spawn
	// shape — still refuses, at every depth: nothing here is attributable.
	c.st.Insert(&childstore.Session{
		ChildID: "c_anon_top", Status: protocol.StatusIdle,
		Labels: map[string]string{"owner": "brent"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_anon_leaf", Status: protocol.StatusIdle,
		Labels: map[string]string{"owner": "brent", childstore.LabelParent: "c_anon_top"},
	})
	for _, id := range []string{"c_anon_top", "c_anon_leaf"} {
		if uid, ok := c.OwnerUserIDForChild(id); ok || uid != "" {
			t.Errorf("OwnerUserIDForChild(%s) = (%q, %v), want a refused anonymous lineage", id, uid, ok)
		}
	}
}

// TestAgentSpawnedGrandchildMCPTokenResolves drives the REAL spawn path: a
// top-level claude child spawned under a user credential, then a grandchild
// spawned THROUGH the controllerSpawner — the exact call the MCP face and a
// fundi child make — pinning that the grandchild's row inherits the subtree's
// owner id and its minted secret resolves. Before the inheritance this is the
// test that failed with a 401-shaped refusal.
//
// Not parallel: it sets CLAUDE_BINARY, and t.Setenv refuses a parallel test.
// The spawner carries no PiBinary (the wire's agent_spawn schema has none),
// so the daemon resolves the binary from the env exactly as production does.
func TestAgentSpawnedGrandchildMCPTokenResolves(t *testing.T) {
	c := newTestController(t)
	// The proxy face must be wired for a claude child to be routed at all —
	// proxyChildEnv is the mint site (see TestChildMCPTokenIsPerChildAndForgotten).
	c.proxyURL, c.proxyToken = "http://127.0.0.1:1/", "boot-secret"
	t.Setenv("CLAUDE_BINARY", fakePiBin(t))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}
	top, err := c.Spawn(ctx, req, users.Identity{UserID: "u_top", Username: "brent"})
	if err != nil {
		t.Fatalf("top-level Spawn: %v", err)
	}

	// The grandchild goes through the controllerSpawner bound to the top
	// child — the exact shape an agent's agent_spawn takes.
	spawner := newControllerSpawner(c, top.ChildID)
	info, err := spawner.Spawn(ctx, tools.SpawnSpec{
		Kind: protocol.KindClaude,
		Name: "inherited-owner",
		Cwd:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("controllerSpawner Spawn: %v", err)
	}

	snap, ok := c.st.Get(info.ChildID)
	if !ok {
		t.Fatal("the grandchild has no store row")
	}
	// The inheritance is the point: the grandchild's row carries the
	// subtree's owner id, read from its spawner's own row — so its MCP secret
	// resolves on its own row, and a fundi descendant's conversation
	// attributes to the top user.
	if snap.OwnerUserID != "u_top" {
		t.Fatalf("agent-spawned grandchild's row carries OwnerUserID %q, want the spawner's own %q — the inheritance did not stamp", snap.OwnerUserID, "u_top")
	}
	if snap.Labels["owner"] != "brent" {
		t.Fatalf("agent-spawned grandchild's owner label = %q, want the parent's propagated %q", snap.Labels["owner"], "brent")
	}

	// The mint-or-reuse contract returns the same secret proxyChildEnv minted
	// during the spawn.
	tok := c.mintMCPToken(info.ChildID)
	if tok == "" {
		t.Fatal("the agent-spawned grandchild minted no MCP secret")
	}
	cid, uid, ok := c.ChildForMCPToken(tok)
	if !ok || cid != info.ChildID || uid != "u_top" {
		t.Fatalf("ChildForMCPToken(grandchild secret) = (%q, %q, %v), want (%q, u_top, true) — the grandchild's own MCP calls would 401", cid, uid, ok, info.ChildID)
	}
}
