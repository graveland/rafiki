// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestConnectChildScopedOnTheConnectPlane is the end-to-end proof of the
// childScoped policy on the REAL daemon: a per-child secret minted at spawn
// (the fake-claude child's RAFIKI_MCP_TOKEN) dialed at the daemon's own
// Connect unix socket must act on its subtree and nothing else.
//
// This also pins the main.go wiring: the policy gate admits the per-child
// credential on childScoped procedures, and if face.Control's
// SetChildScopeSource wiring were dropped the handlers would silently serve
// the OPERATOR path — ListChildren would return the whole fleet instead of
// the subtree, and Kill of a sibling would succeed. The unit fixtures build
// their own wiring; only this test proves the daemon's.
func TestConnectChildScopedOnTheConnectPlane(t *testing.T) {
	t.Parallel()
	d, dumps := bootMCPChildDaemon(t)
	userToken := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, userToken)

	// The caller: a real claude child with a real per-child secret.
	childA := mcpSpawnClaudeChild(t, userSess, "connect-scoped-a")
	dump := waitClaudeDump(t, d, dumps, childA)
	mcpToken := dump.envValue("RAFIKI_MCP_TOKEN")
	if mcpToken == "" {
		t.Fatal("the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")
	}

	// The subtree: one descendant spawned UNDER the caller (operator-seeded,
	// as the cockpit would) and one top-level sibling tree the caller must
	// never reach.
	kid := d.spawnChildUnder(t, childA)
	outsider := d.spawnChild(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := d.connectClient()

	// ListChildren through the child credential: exactly its descendant —
	// never the caller itself, never the outsider.
	lreq := connect.NewRequest(&rafikiv1.ListChildrenRequest{})
	lreq.Header().Set("Authorization", "Bearer "+mcpToken)
	resp, err := client.ListChildren(ctx, lreq)
	if err != nil {
		t.Fatalf("ListChildren as child: %v", err)
	}
	var got []string
	for _, c := range resp.Msg.GetChildren() {
		got = append(got, c.GetChildId())
	}
	if !slices.Equal(got, []string{kid}) {
		t.Fatalf("child ListChildren = %v, want exactly [%s] (its subtree only)", got, kid)
	}

	// Control: the operator credential still sees the whole fleet.
	ureq := connect.NewRequest(&rafikiv1.ListChildrenRequest{})
	ureq.Header().Set("Authorization", "Bearer "+userToken)
	uresp, err := client.ListChildren(ctx, ureq)
	if err != nil {
		t.Fatalf("ListChildren as user: %v", err)
	}
	var seen bool
	for _, c := range uresp.Msg.GetChildren() {
		if c.GetChildId() == outsider {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("user ListChildren omitted the top-level child %s", outsider)
	}

	// Kill of the caller itself: refused — a child is not a descendant of
	// itself.
	self := connect.NewRequest(&rafikiv1.KillRequest{ChildId: childA})
	self.Header().Set("Authorization", "Bearer "+mcpToken)
	if _, err := client.Kill(ctx, self); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("self-Kill as child = %v, want %v", err, connect.CodePermissionDenied)
	}

	// Kill of the sibling tree: refused, and the sibling survives.
	kreq := connect.NewRequest(&rafikiv1.KillRequest{ChildId: outsider})
	kreq.Header().Set("Authorization", "Bearer "+mcpToken)
	if _, err := client.Kill(ctx, kreq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Kill of sibling as child = %v, want %v", err, connect.CodePermissionDenied)
	}

	// Kill of the descendant: allowed, and it really kills.
	kidKill := connect.NewRequest(&rafikiv1.KillRequest{ChildId: kid})
	kidKill.Header().Set("Authorization", "Bearer "+mcpToken)
	if _, err := client.Kill(ctx, kidKill); err != nil {
		t.Fatalf("Kill of own descendant as child: %v", err)
	}
}
