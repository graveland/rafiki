// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// TestControlOwnerIsolationCoversEveryProcedure is the completeness gate for
// owner isolation: every procedure of the generated Control service must be
// classed ownerGuarded, ownerFiltered or ownerExempt, an exempt one must carry
// a reason, and the table must hold no stale names. A new RPC therefore cannot
// land without an owner-isolation decision — the same class of bug the policy
// table's completeness test closes for credentials.
func TestControlOwnerIsolationCoversEveryProcedure(t *testing.T) {
	c := assert.NewCollecting(t)
	svc := rafikiv1.File_rafiki_v1_control_proto.Services().ByName("Control")
	c.Require().NotNil(svc, "no Control service in the generated descriptor")
	methods := svc.Methods()
	classified := make(map[string]bool, methods.Len())
	for i := 0; i < methods.Len(); i++ {
		name := string(methods.Get(i).Name())
		classified[name] = true
		class, ok := ownerIsolationTable[name]
		if !ok {
			t.Errorf("Control.%s has no entry in ownerIsolationTable: classify it before the RPC ships (ownerGuarded if it acts on a child id, ownerFiltered if it lists children, else ownerExempt with a reason)", name)
			continue
		}
		switch class {
		case ownerExempt:
			if ownerExemptReasons[name] == "" {
				t.Errorf("Control.%s is ownerExempt with no entry in ownerExemptReasons: an unjustified exemption is how a leak ships", name)
			}
		case ownerGuarded, ownerFiltered:
		default:
			t.Errorf("Control.%s has an out-of-range owner isolation %d", name, class)
		}
	}
	for name := range ownerIsolationTable {
		c.False(!classified[name], "ownerIsolationTable names %q, which is not a Control service procedure; drop the stale entry", name)
	}
	for name := range ownerExemptReasons {
		c.False(!classified[name], "ownerExemptReasons names %q, which is not a Control service procedure; drop the stale entry", name)
	}
}

// TestOwnerGuardForIdentityShapes pins the source's identity table: only a
// non-admin user credential resolves a guard. Everything else — the unix
// socket's local trust, an admin, a child credential (ChildScope governs), and
// the bare per-boot shape the gate already refused — is nil, i.e. the
// unrestricted path.
func TestOwnerGuardForIdentityShapes(t *testing.T) {
	ctrl := &Controller{st: childstore.New(), cm: newChildManager()}
	for _, tc := range []struct {
		name string
		id   *server.Identity
		want bool
	}{
		{"nil identity (UDS local trust)", nil, false},
		{"admin user credential", &server.Identity{Via: server.ProvenanceUser, UserID: "u1", IsAdmin: true}, false},
		{"non-admin user credential", &server.Identity{Via: server.ProvenanceUser, UserID: "u1"}, true},
		{"per-child credential", &server.Identity{Via: server.ProvenanceChildToken, ChildID: "c_x"}, false},
		{"bare per-boot shape", &server.Identity{}, false},
		{"attributed child credential", &server.Identity{Via: server.ProvenanceChildAttributed, UserID: "u1"}, false},
		{"notify provenance", &server.Identity{Via: server.ProvenanceNotify, UserID: "u1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.id != nil {
				ctx = server.WithIdentity(ctx, tc.id)
			}
			got := ctrl.ownerGuardFor(ctx)
			assert.NewAborting(t).False((got != nil) != tc.want, "ownerGuardFor(%s) = %v, want a guard: %v", tc.name, got, tc.want)
		})
	}
}

// ownerGuardFixture builds a Controller whose childstore holds one row per
// ownership shape the guard must decide.
func ownerGuardFixture() *Controller {
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{
		ChildID: "c_mine", Status: protocol.StatusIdle, OwnerUserID: "u1",
	})
	// An agent-spawned descendant: stamped, and labelled under its parent.
	c.st.Insert(&childstore.Session{
		ChildID: "c_kid", Status: protocol.StatusIdle, OwnerUserID: "u1",
		Labels: map[string]string{childstore.LabelParent: "c_mine", childstore.LabelRoot: "c_mine"},
	})
	// A legacy pre-inheritance row: no stamp of its own, but its lineage has
	// an owner (c_mine), so the chain walk attributes it to u1. The
	// admin/UDS-only rule is about a lineage with NO owner anywhere — see
	// c_anon below — not about a NULL stamp on an owned subtree.
	c.st.Insert(&childstore.Session{
		ChildID: "c_legacy", Status: protocol.StatusIdle,
		Labels: map[string]string{childstore.LabelParent: "c_mine"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_theirs", Status: protocol.StatusIdle, OwnerUserID: "u2",
	})
	// The anonymous UDS spawn shape: no stamp AND no parent, so no owner
	// anywhere in the lineage — admin/UDS-only, refused to every user.
	c.st.Insert(&childstore.Session{ChildID: "c_anon", Status: protocol.StatusIdle})
	return c
}

// TestOwnerGuardAuthorize pins the refusal: a guard bound to u1 admits its own
// rows — including a legacy row with no stamp of its own whose lineage has an
// owner — and refuses a foreign child, a lineage with no owner anywhere (the
// admin/UDS-only shape), an unknown id and the empty id, all with the ONE
// identical PermissionDenied text so no id can be probed.
func TestOwnerGuardAuthorize(t *testing.T) {
	c := assert.NewCollecting(t)
	g := ownerGuard{c: ownerGuardFixture(), userID: "u1"}

	for _, id := range []string{"c_mine", "c_kid", "c_legacy"} {
		err := g.Authorize(id)
		c.NoError(err, "Authorize(%s) = %v, want admitted", id, err)
		c.True(g.Owns(id), "Owns(%s) = false, want true (Authorize admits it)", id)
	}

	refused := []string{"c_theirs", "c_anon", "c_unknown", ""}
	var first string
	for i, id := range refused {
		err := g.Authorize(id)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Authorize(%q) code = %v, want PermissionDenied", id, err)
		c.False(g.Owns(id), "Owns(%q) = true for a refused id", id)
		if err == nil {
			continue
		}
		msg := err.Error()
		if i == 0 {
			first = msg
			c.False(!strings.Contains(msg, "is not owned by you"), "refusal %q does not carry the owner text", msg)
			continue
		}
		c.Eq(strings.ReplaceAll(first, refused[0], id), msg, "refusal for %q differs from the first refusal; foreign, unowned, unknown and empty ids must be indistinguishable", id)
	}
}

// TestOwnerGuardOwnsConversation pins ListTasks' boundary: a conversation is
// owned only when it resolves to a live child row the caller owns. A fundi
// row carries its conversation UUID on SessionID (see
// Controller.conversationIDForChild), so no database is needed here.
func TestOwnerGuardOwnsConversation(t *testing.T) {
	c := assert.NewCollecting(t)
	ctrl := ownerGuardFixture()
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_conv", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		OwnerUserID: "u1", SessionID: "0f3c5d9e-1111-4a2b-9c3d-000000000001",
	})
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_conv_theirs", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		OwnerUserID: "u2", SessionID: "0f3c5d9e-2222-4a2b-9c3d-000000000002",
	})
	g := ownerGuard{c: ctrl, userID: "u1"}

	c.True(g.OwnsConversation("0f3c5d9e-1111-4a2b-9c3d-000000000001"), "an owned fundi conversation is not owned")
	c.False(g.OwnsConversation("0f3c5d9e-2222-4a2b-9c3d-000000000002"), "a foreign child's conversation is owned")
	c.False(g.OwnsConversation("no-such-conversation"), "an unresolvable conversation is owned")
	c.False(g.OwnsConversation(""), "the empty conversation id is owned")
}

// TestOwnerGuardRefusalTextIsTheDocumentedOne pins the exact sentence the
// design promises: one text, naming the id, for every refusal. Clients match
// on it, and the integration test compares refusals across ids.
func TestOwnerGuardRefusalTextIsTheDocumentedOne(t *testing.T) {
	g := ownerGuard{c: ownerGuardFixture(), userID: "u1"}
	err := g.Authorize("c_theirs")
	assert.NewAborting(t).Eq(
		"permission_denied: agent c_theirs is not owned by you; you may only view, steer or kill agents you own",
		err.Error(), "refusal text")
	var cerr *connect.Error
	assert.NewAborting(t).True(errors.As(err, &cerr), "refusal is not a *connect.Error")
}
