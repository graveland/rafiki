// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os/user"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// adminFixture builds the adapter over a Controller with the in-memory
// executor store (see fakeExecStore in controller_executors_test.go), so the
// adapter's delegation is exercised through the real Controller methods.
func adminFixture() (connectExecutorAdmin, *fakeExecStore) {
	s := newFakeExecStore()
	return connectExecutorAdmin{c: &Controller{execStore: s}}, s
}

// TestExecutorAdminEnrollStampsOwnerFromTheConnection pins the identity rule:
// the owner comes from the connection's identity via the request CONTEXT, so
// the minted token's trust labels carry the caller's username — never anything
// the request could name (it has no owner field, and executorTrustLabels
// refuses one in labels).
func TestExecutorAdminEnrollStampsOwnerFromTheConnection(t *testing.T) {
	c := assert.NewAborting(t)
	a, s := adminFixture()
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	resp, err := a.Enroll(ctx, &rafikiv1.EnrollExecutorRequest{
		Name: "laptop", Labels: map[string]string{"env": "dev"}, TtlSeconds: 3600,
	})
	c.NoError(err)
	c.NotEq("", resp.Token, "enroll returned an empty token")
	c.Len(s.minted, 1, "want one minted token, got %d", len(s.minted))
	got := s.minted[0].Labels
	c.Eq("brent", got["owner"], `labels["owner"] = %q, want "brent" from the connection identity`, got["owner"])
	c.False(got["machine"] != "laptop" || got["env"] != "dev", "stamped labels lost the request's own: %+v", got)
}

// TestExecutorAdminCreateStampsOwnerFromTheConnection is the same pin on the
// stateless path: the created row's labels carry the connection's owner, and
// the row id plus shown-once credential ride out to the client.
func TestExecutorAdminCreateStampsOwnerFromTheConnection(t *testing.T) {
	c := assert.NewAborting(t)
	a, s := adminFixture()
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	resp, err := a.Create(ctx, &rafikiv1.CreateExecutorRequest{Name: "laptop"})
	c.NoError(err)
	c.False(resp.ExecutorId == "" || resp.Credential == "", "response = %+v, want a row id and a credential", resp)
	c.Eq("brent", s.lastCreate.Labels["owner"], `created labels["owner"] = %q, want "brent" from the connection identity`, s.lastCreate.Labels["owner"])
}

// TestExecutorAdminEnrollRefusesAClientSuppliedOwner pins the other half of the
// rule end to end through the adapter: even with a valid connection identity,
// a request that tries to write `owner` through its labels is refused — the
// adapter must not launder the labels past executorTrustLabels.
func TestExecutorAdminEnrollRefusesAClientSuppliedOwner(t *testing.T) {
	a, s := adminFixture()
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	_, err := a.Enroll(ctx, &rafikiv1.EnrollExecutorRequest{
		Labels:     map[string]string{"owner": "mallory"},
		TtlSeconds: 3600,
	})
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs {
		t.Fatalf("got %v, want a ControllerError with code %s", err, protocol.ErrInvalidArgs)
	}
	assert.NewAborting(t).Empty(s.minted, "a refused request minted %d tokens", len(s.minted))
}

// TestExecutorAdminNilIdentityFallsBackToTheDaemonOSUser pins the nil branch:
// no identity on the context (the UDS local-trust case) becomes
// users.Identity{}, whose owner sessionOwner resolves to the daemon's own OS
// user — the same value connIdentity hands the framed dispatcher.
func TestExecutorAdminNilIdentityFallsBackToTheDaemonOSUser(t *testing.T) {
	c := assert.NewAborting(t)
	a, s := adminFixture()

	if _, err := a.Enroll(context.Background(), &rafikiv1.EnrollExecutorRequest{TtlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	want, err := user.Current()
	c.NoError(err, "resolving the daemon's OS user")
	got := s.minted[0].Labels["owner"]
	c.Eq(want.Username, got, `labels["owner"] = %q, want the daemon's OS user %q`, got, want.Username)
}

// adminListStore stuffs n enabled rows into the fake store, enough to exercise
// the limit contract's default and clamp.
func adminListStore(n int) *fakeExecStore {
	s := newFakeExecStore()
	for i := 0; i < n; i++ {
		id := "exec-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		s.execs[id] = executors.Executor{ID: id, Enabled: true, Labels: map[string]string{"env": "work"}}
	}
	return s
}

// TestExecutorAdminListCarriesTheFramedLimitSemantics pins the limit
// normalization: an absent (0) limit becomes the framed default of 50, an
// oversized one is clamped to the framed max of 500, and an explicit one is
// honored as-is. Passing 0 through to Controller.ExecutorList would mean "no
// cap", turning an omitted field into an unbounded listing. The 50/500
// expectations are literals — dispatch.go's maxExecutorListLimit and its
// `Limit <= 0 → 50` default — NOT the adapter's executorListDefaultLimit/
// executorListMaxLimit consts, so a const change fails here instead of
// following it.
func TestExecutorAdminListCarriesTheFramedLimitSemantics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  int
		limit int32
		want  int
	}{
		{"absent limit gets the framed default", 60, 0, 50},
		{"explicit limit is honored", 60, 7, 7},
		{"oversized limit is clamped", 600, 700, 500},
		{"limit above the row count is inert", 3, 50, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			a := connectExecutorAdmin{c: &Controller{execStore: adminListStore(tc.rows)}}
			rows, err := a.List(context.Background(), "", tc.limit)
			ck.Require().NoError(err)
			ck.Len(rows, tc.want, "limit %d over %d rows: got %d rows, want", tc.limit, tc.rows, len(rows))
		})
	}
}

// TestExecutorAdminListHonoursTheSelector pins the selector pass-through: the
// framed selector semantics (label match, invalid selector refused) belong to
// Controller.ExecutorList, and the adapter forwards the field untouched.
func TestExecutorAdminListHonoursTheSelector(t *testing.T) {
	ck := assert.NewCollecting(t)
	// adminListStore labels every row env=work, so a matching selector keeps
	// all of them (60 > the 50-row default, which also pins that the selector
	// path does not disturb the limit contract).
	a := connectExecutorAdmin{c: &Controller{execStore: adminListStore(60)}}

	rows, err := a.List(context.Background(), "env=work", 0)
	ck.Require().NoError(err)
	ck.Len(rows, executorListDefaultLimit, "selector matching every row: got %d rows, want the 50-row default", len(rows))

	// "os in linux" is a broken compound (set membership without parentheses),
	// which ParseSelector refuses.
	_, err = a.List(context.Background(), "os in linux", 0)
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs {
		t.Errorf("got %v, want a ControllerError with code %s", err, protocol.ErrInvalidArgs)
	}
}

// TestExecutorAdminListLeavesEligibilityUnevaluated pins the row mapping for
// the plain listing: the fields the store row carries ride across, and the
// eligibility fields stay zero — this path must not evaluate, or appear to
// have evaluated, whether an executor could serve a spawn.
func TestExecutorAdminListLeavesEligibilityUnevaluated(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newFakeExecStore()
	s.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "laptop", "env": "work"},
		Isolation: "container", WorkspaceMode: "workspace",
		Roots: []string{"/home/brent"}, Admits: "kind=claude", Enabled: true,
	}
	a := connectExecutorAdmin{c: &Controller{execStore: s}}

	rows, err := a.List(context.Background(), "", 0)
	ck.Require().NoError(err)
	ck.Require().Len(rows, 1, "got %d rows, want 1", len(rows))
	r := rows[0]
	ck.False(r.ID != "exec-1" || r.Machine != "laptop" || r.Labels["env"] != "work" ||
		r.Isolation != "container" || r.WorkspaceMode != "workspace" ||
		len(r.Roots) != 1 || r.Roots[0] != "/home/brent" || r.Admits != "kind=claude" || !r.Enabled, "row mapping lost a field: %+v", r)
	ck.Nil(r.LaunchKinds, "plain listing set launch kinds")
	ck.False(r.Eligible, "plain listing set eligible")
	ck.Eq("", r.Reason, `plain listing set reason %q`, r.Reason)
}

// TestExecutorAdminListMarksConnectedFromTheLivePool pins the Connected
// pass-through: Controller.ExecutorList marks it from the live pool, and the
// adapter must carry the mark onto the face's row.
func TestExecutorAdminListMarksConnectedFromTheLivePool(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newFakeExecStore()
	s.execs["exec-live"] = executors.Executor{ID: "exec-live", Enabled: true}
	s.execs["exec-off"] = executors.Executor{ID: "exec-off", Enabled: true}
	live := ex("exec-live", nil, "")
	a := connectExecutorAdmin{c: &Controller{
		execStore: s,
		execPool:  &fakePool{live: []execpool.LiveExecutor{live}},
	}}

	rows, err := a.List(context.Background(), "", 0)
	ck.Require().NoError(err)
	byID := map[string]connectapi.ExecutorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	ck.True(byID["exec-live"].Connected, "exec-live not marked connected though the pool has it live")
	ck.False(byID["exec-off"].Connected, "exec-off marked connected though the pool does not have it")
}

// TestExecutorAdminListCarriesConnectionTimestamps pins the timestamp
// pass-through on the empty-kind path: connected_at_ms comes from the live
// pool's join time and is 0 for an executor with no current connection
// (ConnectedAt nil), and last_seen_ms mirrors the store's last_seen_at — set
// when the row carries one, 0 when the column is NULL (the Go zero time: an
// executor never seen).
func TestExecutorAdminListCarriesConnectionTimestamps(t *testing.T) {
	ck := assert.NewCollecting(t)
	joined := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	seen := joined.Add(-time.Hour)
	s := newFakeExecStore()
	s.execs["exec-live"] = executors.Executor{
		ID: "exec-live", Enabled: true, LastSeenAt: seen,
	}
	s.execs["exec-off"] = executors.Executor{ID: "exec-off", Enabled: true}
	live := ex("exec-live", nil, "")
	live.ConnectedAt = joined
	a := connectExecutorAdmin{c: &Controller{
		execStore: s,
		execPool:  &fakePool{live: []execpool.LiveExecutor{live}},
	}}

	rows, err := a.List(context.Background(), "", 0)
	ck.Require().NoError(err)
	byID := map[string]connectapi.ExecutorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	ck.True(byID["exec-live"].Connected, "exec-live not marked connected though the pool has it live")
	if got, want := byID["exec-live"].ConnectedAtMs, joined.UnixMilli(); got != want {
		t.Errorf("exec-live connected_at_ms = %d, want the pool's join time %d", got, want)
	}
	if got, want := byID["exec-live"].LastSeenMs, seen.UnixMilli(); got != want {
		t.Errorf("exec-live last_seen_ms = %d, want the store row's sighting %d", got, want)
	}
	ck.Eq(0, byID["exec-off"].ConnectedAtMs, "exec-off connected_at_ms")
	ck.Eq(0, byID["exec-off"].LastSeenMs, "exec-off last_seen_ms")
}

// TestExecutorAdminLabelMapsTheRow pins the single-row mapping: the updated
// row ExecutorLabel returns becomes the face's row, machine label included.
func TestExecutorAdminLabelMapsTheRow(t *testing.T) {
	c := assert.NewCollecting(t)
	a, s := adminFixture()
	s.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "laptop"}, Enabled: true,
	}

	row, err := a.Label(context.Background(), &rafikiv1.LabelExecutorRequest{
		ExecutorId: "exec-1", Set: map[string]string{"rack": "r1"},
	})
	c.Require().NoError(err)
	c.False(row.ID != "exec-1" || row.Machine != "laptop" || !row.Enabled || row.Labels["rack"] != "r1", "row = %+v, want the relabelled store row", row)
}

// TestExecutorAdminErrorsPassThroughUnmapped pins that the adapter does NOT
// map errors: a store not-found stays the Controller's ControllerError for the
// connectapi handler to classify. Mapping here would hand the handler an
// already-mapped connect error, which its own ConnectErr pass would
// re-classify as Internal and lose the code.
func TestExecutorAdminErrorsPassThroughUnmapped(t *testing.T) {
	c := assert.NewCollecting(t)
	a, s := adminFixture()

	err := a.Disable(context.Background(), "no-such-executor")
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrNotFound {
		t.Fatalf("got %v, want a ControllerError with code %s", err, protocol.ErrNotFound)
	}

	s.execs["exec-1"] = executors.Executor{ID: "exec-1", Enabled: true}
	c.Require().NoError(a.Delete(context.Background(), "exec-1"))
	_, ok := s.execs["exec-1"]
	c.False(ok, "delete did not reach the store")
}
