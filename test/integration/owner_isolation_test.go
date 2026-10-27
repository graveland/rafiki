// SPDX-License-Identifier: Apache-2.0

package integration_test

// Owner isolation on the Control plane, end to end against a REAL daemon.
//
// The policy gate admits every user credential to every Control procedure, so
// the per-user boundary lives in the handlers, behind connectapi's
// OwnerGuardSource, wired in main.go to Controller.ownerGuardFor. A unit
// fixture builds its own wiring; only this test proves the daemon's: two
// ordinary users plus an admin plus the anonymous unix socket, per verb, with
// state proven unchanged rather than only the error code checked.
//
// Same rules as mcp_test.go: -count=1 always, because go test caching cannot
// see through to a daemon subprocess.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// ownerIsolationSpawn spawns a fundi child AS token — the daemon stamps the
// child's owner from the credential the unix-socket mount resolved.
func ownerIsolationSpawn(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, token, parent string) string {
	t.Helper()
	c := assert.NewAborting(t)
	resp, err := client.Spawn(ctx, authorize(token, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		NoSession:     true,
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		ParentChildId: parent,
	})))
	c.NoError(err, "Spawn as %s", token)
	c.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

// ownerIsolationSpawnErr is ownerIsolationSpawn for the refusal arms: it
// returns the error and whatever child id came back.
func ownerIsolationSpawnErr(ctx context.Context, client rafikiv1connect.ControlClient, token, parent string) (string, error) {
	resp, err := client.Spawn(ctx, authorize(token, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		NoSession:     true,
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		ParentChildId: parent,
	})))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetChildId(), nil
}

// ownerIsolationIDs maps a ListChildren answer to its child ids.
func ownerIsolationIDs(resp *rafikiv1.ListChildrenResponse) []string {
	out := make([]string, 0, len(resp.GetChildren()))
	for _, c := range resp.GetChildren() {
		out = append(out, c.GetChildId())
	}
	return out
}

// TestConnectOwnerIsolationOnTheConnectPlane is the end-to-end proof of owner
// isolation between users on the Control plane.
func TestConnectOwnerIsolationOnTheConnectPlane(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d := bootDaemonDB(t, nextDaemonID())
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))

	tokenA := d.createMCPUser(t)
	tokenB := d.createMCPUser(t)
	// CreateUser can never mint an admin (by design), so the admin comes from
	// the store directly — the same route the recall tests use.
	_, adminToken := createRecallUser(t, pool, "owner-iso-admin-"+nextDaemonID(), true)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client := d.connectClient()

	// The test's own children, killed on cleanup. The harness's daemon
	// teardown handles the rest; this keeps the fleet small enough that the
	// UDS/admin "sees everything" arms stay cheap.
	var spawned []string
	kill := func(token, id string) {
		if id == "" {
			return
		}
		_, _ = client.Kill(ctx, authorize(token, connect.NewRequest(&rafikiv1.KillRequest{ChildId: id})))
	}
	t.Cleanup(func() {
		for _, id := range spawned {
			kill("", id)
		}
	})

	// ── 1. Two users, one child each; A reads its own ──────────────────────
	childA := ownerIsolationSpawn(t, ctx, client, tokenA, "")
	spawned = append(spawned, childA)
	if _, err := client.GetChild(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childA}))); err != nil {
		t.Fatalf("GetChild(childA) as A = %v, want success", err)
	}
	childB := ownerIsolationSpawn(t, ctx, client, tokenB, "")
	spawned = append(spawned, childB)

	// ── 2. Every guarded verb refuses B on A's child, state unchanged ──────
	before := getChild(t, client, childA)
	beforeLabels := before.GetLabels()

	foreign := []struct {
		name   string
		invoke func() error
	}{
		{"GetChild", func() error {
			_, err := client.GetChild(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childA})))
			return err
		}},
		{"Kill", func() error {
			_, err := client.Kill(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childA})))
			return err
		}},
		{"SetBudget", func() error {
			_, err := client.SetBudget(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: childA, MaxCost: 0})))
			return err
		}},
		{"Send", func() error {
			_, err := client.Send(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: childA, Mode: rafikiv1.SendMode_SEND_MODE_PROMPT,
				Blocks: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}}}},
			})))
			return err
		}},
		{"Rename", func() error {
			_, err := client.Rename(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.RenameRequest{ChildId: childA, Name: "hijacked"})))
			return err
		}},
		{"SetLabels", func() error {
			_, err := client.SetLabels(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.SetLabelsRequest{
				ChildId: childA, Set: map[string]string{"pwned": "yes"},
			})))
			return err
		}},
		{"Resume", func() error {
			_, err := client.Resume(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: childA})))
			return err
		}},
		{"GetHistory", func() error {
			_, err := client.GetHistory(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childA})))
			return err
		}},
		{"GetStreams", func() error {
			_, err := client.GetStreams(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: childA})))
			return err
		}},
		{"SetRouting", func() error {
			_, err := client.SetRouting(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: childA, Delta: "prefer=x"})))
			return err
		}},
	}
	for _, verb := range foreign {
		ck.Eq(connect.CodePermissionDenied, connect.CodeOf(verb.invoke()),
			"%s of A's child as B must be refused; got", verb.name)
	}
	// State, not just the error: nothing was renamed, labelled or killed.
	after := getChild(t, client, childA)
	ck.Eq(before.GetName(), after.GetName(), "A's child was renamed by B")
	ck.Eq(string(protocol.StatusIdle), after.GetStatus(), "A's child left its status; got %s", after.GetStatus())
	for k, v := range beforeLabels {
		ck.Eq(v, after.GetLabels()[k], "label %q changed under B", k)
	}
	ck.Eq("", after.GetLabels()["pwned"], "B's label landed on A's child")

	// ── 3. Foreign and unknown ids are indistinguishable ───────────────────
	_, foreignErr := client.GetChild(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childA})))
	_, unknownErr := client.GetChild(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_no_such_child"})))
	ck.Require().NotNil(foreignErr, "GetChild(childA) as B did not fail")
	ck.Require().NotNil(unknownErr, "GetChild(unknown) as B did not fail")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(unknownErr), "unknown id code")
	ck.Eq(strings.ReplaceAll(foreignErr.Error(), childA, "c_no_such_child"), unknownErr.Error(),
		"foreign and unknown ids must answer identically, or ids can be probed")

	// ── 4. Spawn under a foreign parent is refused ─────────────────────────
	gotID, err := ownerIsolationSpawnErr(ctx, client, tokenB, childA)
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Spawn under A's child as B = %v, want PermissionDenied", err)
	ck.Eq("", gotID, "the refused spawn returned a child id")
	own := ownerIsolationSpawn(t, ctx, client, tokenB, childB)
	spawned = append(spawned, own)

	// ── 5. Lists are disjoint; UDS and admin see everything ────────────────
	unowned := d.spawnChild(t)
	spawned = append(spawned, unowned)

	listAs := func(token string) []string {
		resp, err := client.ListChildren(ctx, authorize(token, connect.NewRequest(&rafikiv1.ListChildrenRequest{})))
		ck.NoError(err, "ListChildren as %q", token)
		return ownerIsolationIDs(resp.Msg)
	}
	asA := listAs(tokenA)
	ck.Contains(asA, childA)
	ck.NotContains(asA, childB)
	ck.NotContains(asA, unowned)
	asB := listAs(tokenB)
	ck.Contains(asB, childB)
	ck.NotContains(asB, childA)
	ck.NotContains(asB, unowned)
	for _, id := range []string{childA, childB, unowned} {
		ck.Contains(listAs(""), id, "the anonymous unix socket must still see the whole fleet")
		ck.Contains(listAs(adminToken), id, "an admin credential must still see the whole fleet")
	}

	// ── 6. NULL-owner children are admin/UDS-only ──────────────────────────
	_, err = client.GetChild(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: unowned})))
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "GetChild(unowned) as B = %v, want PermissionDenied", err)
	if _, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: unowned})); err != nil {
		t.Fatalf("GetChild(unowned) over the UDS = %v, want success", err)
	}
	if _, err := client.GetChild(ctx, authorize(adminToken, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: unowned}))); err != nil {
		t.Fatalf("GetChild(unowned) as admin = %v, want success", err)
	}

	// ── 7. Status counts cover the caller's own children only ──────────────
	// B has two live children of its own right now; A's and the unowned child
	// must not appear in its totals. Expected counts are computed from what
	// this test spawned and B still owns — never from a global row count.
	statusAs := func(token string) *rafikiv1.StatusResponse {
		resp, err := client.Status(ctx, authorize(token, connect.NewRequest(&rafikiv1.StatusRequest{})))
		ck.NoError(err, "Status as %q", token)
		return resp.Msg
	}
	bs := statusAs(tokenB).GetChildren()
	ck.Eq(int32(2), bs.GetLive(), "B's live count = %d, want 2 (its own children only)", bs.GetLive())
	ck.Eq(int32(0), bs.GetExited(), "B's exited count = %d, want 0", bs.GetExited())

	// ── 8. CloseAllExited closes only the caller's children ────────────────
	kill(tokenA, childA)
	kill(tokenB, childB)
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(func() error {
		_, err := client.GetHistory(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childA})))
		return err
	}()), "B still reaches A's exited child")

	closed, err := client.CloseAllExited(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.CloseAllExitedRequest{})))
	ck.NoError(err, "CloseAllExited as B")
	ck.Contains(closed.Msg.GetChildIds(), childB, "B's own exited child was not closed")
	ck.NotContains(closed.Msg.GetChildIds(), childA, "A's child was closed by B")
	if _, err := client.GetChild(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childA}))); err != nil {
		t.Fatalf("A's child is gone after B's CloseAllExited: %v", err)
	}
	closedA, err := client.CloseAllExited(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.CloseAllExitedRequest{})))
	ck.NoError(err, "CloseAllExited as A")
	ck.Contains(closedA.Msg.GetChildIds(), childA, "A's own CloseAllExited did not close its child")

	// Ownership resolves through the LIVE childstore, so a closed child is no
	// longer owned — by anyone but the admin and the unix socket (see "Owner
	// isolation between users" in docs/reference/control-protocol.md, rule 7).
	// Pinned here because it is the one visible cost of the boundary: the row
	// is gone, and `GetHistory` refuses it exactly as it refuses an unknown id.
	_, err = client.GetHistory(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childA})))
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err),
		"GetHistory of A's own CLOSED child = %v, want PermissionDenied (closed rows carry no owner)", err)

	// ── 9. StreamEvents: the All subject carries only owned children ───────
	streamEventsAllAsB(t, ctx, client, tokenA, tokenB, &spawned)

	// ── 10. Admin and the UDS keep full authority ──────────────────────────
	childA2 := ownerIsolationSpawn(t, ctx, client, tokenA, "")
	spawned = append(spawned, childA2)
	if _, err := client.Kill(ctx, authorize(adminToken, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childA2}))); err != nil {
		t.Fatalf("Kill(A's child) as admin = %v, want success", err)
	}
}

// streamEventsAllAsB pins the filtered stream: with the All subject, B's
// stream carries its own children's events and never A's.
//
// A bounded read: the stream is followed for up to 5s, collecting whatever
// arrives. Fundi children publish status events on spawn and exit, which is
// what this drives. If the harness's children published nothing on this path
// the arm would prove nothing — so the test requires at least one event of
// B's own before it judges the absence of A's.
func streamEventsAllAsB(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, tokenA, tokenB string, spawned *[]string) {
	t.Helper()
	ck := assert.NewAborting(t)

	streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream, err := client.StreamEvents(streamCtx, authorize(tokenB, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{}},
		// A floor cursor of now: no replay, live events only.
		Cursor: &rafikiv1.EventCursor{Floor: timestamppb.New(time.Now())},
	})))
	ck.NoError(err, "StreamEvents(All) as B")

	childA2 := ownerIsolationSpawn(t, ctx, client, tokenA, "")
	*spawned = append(*spawned, childA2)
	childB2 := ownerIsolationSpawn(t, ctx, client, tokenB, "")
	*spawned = append(*spawned, childB2)

	// Exit both, which is what makes each child publish its final events.
	_, _ = client.Kill(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childA2})))
	_, _ = client.Kill(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childB2})))

	sawOwn := false
	for stream.Receive() {
		ev := stream.Msg()
		ck.NotEq(childA2, ev.GetChildId(), "B's All-subject stream carried A's child's event")
		if ev.GetChildId() == childB2 {
			sawOwn = true
		}
	}
	ck.True(sawOwn, "B's All-subject stream saw none of its own child's events, so the absence of A's proves nothing")
}
