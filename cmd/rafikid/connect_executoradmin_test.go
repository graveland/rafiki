// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os/user"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
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
	a, s := adminFixture()
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	resp, err := a.Enroll(ctx, &rafikiv1.EnrollExecutorRequest{
		Name: "laptop", Labels: map[string]string{"env": "dev"}, TtlSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Fatal("enroll returned an empty token")
	}
	if len(s.minted) != 1 {
		t.Fatalf("want one minted token, got %d", len(s.minted))
	}
	got := s.minted[0].Labels
	if got["owner"] != "brent" {
		t.Fatalf(`labels["owner"] = %q, want "brent" from the connection identity`, got["owner"])
	}
	if got["machine"] != "laptop" || got["env"] != "dev" {
		t.Fatalf("stamped labels lost the request's own: %+v", got)
	}
}

// TestExecutorAdminCreateStampsOwnerFromTheConnection is the same pin on the
// stateless path: the created row's labels carry the connection's owner, and
// the row id plus shown-once credential ride out to the client.
func TestExecutorAdminCreateStampsOwnerFromTheConnection(t *testing.T) {
	a, s := adminFixture()
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	resp, err := a.Create(ctx, &rafikiv1.CreateExecutorRequest{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExecutorId == "" || resp.Credential == "" {
		t.Fatalf("response = %+v, want a row id and a credential", resp)
	}
	if s.lastCreate.Labels["owner"] != "brent" {
		t.Fatalf(`created labels["owner"] = %q, want "brent" from the connection identity`,
			s.lastCreate.Labels["owner"])
	}
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
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs {
		t.Fatalf("got %v, want a ControllerError with code %s", err, protocol.ErrInvalidArgs)
	}
	if len(s.minted) != 0 {
		t.Fatalf("a refused request minted %d tokens", len(s.minted))
	}
}

// TestExecutorAdminNilIdentityFallsBackToTheDaemonOSUser pins the nil branch:
// no identity on the context (the UDS local-trust case) becomes
// users.Identity{}, whose owner sessionOwner resolves to the daemon's own OS
// user — the same value connIdentity hands the framed dispatcher.
func TestExecutorAdminNilIdentityFallsBackToTheDaemonOSUser(t *testing.T) {
	a, s := adminFixture()

	if _, err := a.Enroll(context.Background(), &rafikiv1.EnrollExecutorRequest{TtlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	want, err := user.Current()
	if err != nil {
		t.Fatalf("resolving the daemon's OS user: %v", err)
	}
	if got := s.minted[0].Labels["owner"]; got != want.Username {
		t.Fatalf(`labels["owner"] = %q, want the daemon's OS user %q`, got, want.Username)
	}
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
			a := connectExecutorAdmin{c: &Controller{execStore: adminListStore(tc.rows)}}
			rows, err := a.List(context.Background(), "", tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.want {
				t.Errorf("limit %d over %d rows: got %d rows, want %d", tc.limit, tc.rows, len(rows), tc.want)
			}
		})
	}
}

// TestExecutorAdminListHonoursTheSelector pins the selector pass-through: the
// framed selector semantics (label match, invalid selector refused) belong to
// Controller.ExecutorList, and the adapter forwards the field untouched.
func TestExecutorAdminListHonoursTheSelector(t *testing.T) {
	// adminListStore labels every row env=work, so a matching selector keeps
	// all of them (60 > the 50-row default, which also pins that the selector
	// path does not disturb the limit contract).
	a := connectExecutorAdmin{c: &Controller{execStore: adminListStore(60)}}

	rows, err := a.List(context.Background(), "env=work", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != executorListDefaultLimit {
		t.Errorf("selector matching every row: got %d rows, want the 50-row default", len(rows))
	}

	// "os in linux" is a broken compound (set membership without parentheses),
	// which ParseSelector refuses.
	_, err = a.List(context.Background(), "os in linux", 0)
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs {
		t.Errorf("got %v, want a ControllerError with code %s", err, protocol.ErrInvalidArgs)
	}
}

// TestExecutorAdminListLeavesEligibilityUnevaluated pins the row mapping for
// the plain listing: the fields the store row carries ride across, and the
// eligibility fields stay zero — this path must not evaluate, or appear to
// have evaluated, whether an executor could serve a spawn.
func TestExecutorAdminListLeavesEligibilityUnevaluated(t *testing.T) {
	s := newFakeExecStore()
	s.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "laptop", "env": "work"},
		Isolation: "container", WorkspaceMode: "workspace",
		Roots: []string{"/home/brent"}, Admits: "kind=claude", Enabled: true,
	}
	a := connectExecutorAdmin{c: &Controller{execStore: s}}

	rows, err := a.List(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "exec-1" || r.Machine != "laptop" || r.Labels["env"] != "work" ||
		r.Isolation != "container" || r.WorkspaceMode != "workspace" ||
		len(r.Roots) != 1 || r.Roots[0] != "/home/brent" || r.Admits != "kind=claude" || !r.Enabled {
		t.Errorf("row mapping lost a field: %+v", r)
	}
	if r.LaunchKinds != nil {
		t.Errorf("plain listing set launch kinds: %+v", r.LaunchKinds)
	}
	if r.Eligible {
		t.Error("plain listing set eligible")
	}
	if r.Reason != "" {
		t.Errorf(`plain listing set reason %q`, r.Reason)
	}
}

// TestExecutorAdminListMarksConnectedFromTheLivePool pins the Connected
// pass-through: Controller.ExecutorList marks it from the live pool, and the
// adapter must carry the mark onto the face's row.
func TestExecutorAdminListMarksConnectedFromTheLivePool(t *testing.T) {
	s := newFakeExecStore()
	s.execs["exec-live"] = executors.Executor{ID: "exec-live", Enabled: true}
	s.execs["exec-off"] = executors.Executor{ID: "exec-off", Enabled: true}
	live := ex("exec-live", nil, "")
	a := connectExecutorAdmin{c: &Controller{
		execStore: s,
		execPool:  &fakePool{live: []execpool.LiveExecutor{live}},
	}}

	rows, err := a.List(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]connectapi.ExecutorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if !byID["exec-live"].Connected {
		t.Error("exec-live not marked connected though the pool has it live")
	}
	if byID["exec-off"].Connected {
		t.Error("exec-off marked connected though the pool does not have it")
	}
}

// TestExecutorAdminLabelMapsTheRow pins the single-row mapping: the updated
// row ExecutorLabel returns becomes the face's row, machine label included.
func TestExecutorAdminLabelMapsTheRow(t *testing.T) {
	a, s := adminFixture()
	s.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "laptop"}, Enabled: true,
	}

	row, err := a.Label(context.Background(), &rafikiv1.LabelExecutorRequest{
		ExecutorId: "exec-1", Set: map[string]string{"rack": "r1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "exec-1" || row.Machine != "laptop" || !row.Enabled || row.Labels["rack"] != "r1" {
		t.Errorf("row = %+v, want the relabelled store row", row)
	}
}

// TestExecutorAdminErrorsPassThroughUnmapped pins that the adapter does NOT
// map errors: a store not-found stays the Controller's ControllerError for the
// connectapi handler to classify. Mapping here would hand the handler an
// already-mapped connect error, which its own ConnectErr pass would
// re-classify as Internal and lose the code.
func TestExecutorAdminErrorsPassThroughUnmapped(t *testing.T) {
	a, s := adminFixture()

	err := a.Disable(context.Background(), "no-such-executor")
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrNotFound {
		t.Fatalf("got %v, want a ControllerError with code %s", err, protocol.ErrNotFound)
	}

	s.execs["exec-1"] = executors.Executor{ID: "exec-1", Enabled: true}
	if err := a.Delete(context.Background(), "exec-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.execs["exec-1"]; ok {
		t.Error("delete did not reach the store")
	}
}
