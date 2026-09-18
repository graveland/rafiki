// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/server"
)

// TestConnectPyModulesOwnerFromContext pins the security-relevant rule: the
// owner is resolved from the request CONTEXT, never from a request field. A
// ctx with no identity writes the shared unattributed bucket (owner ""), a
// ctx carrying an identity writes that identity's bucket, and a later List
// under the same identity sees only that owner's rows.
func TestConnectPyModulesOwnerFromContext(t *testing.T) {
	f := newPymoduleFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store}}

	// No identity on the ctx: the anonymous unix-socket case. The write lands
	// in the empty-owner bucket -- the shared unattributed bucket, never
	// "global".
	if _, err := m.PutPymodule(context.Background(), "shared_util", "x = 1", "nobody's util"); err != nil {
		t.Fatalf("PutPymodule with no identity: %v", err)
	}
	rows := f.store.rows[""]
	if len(rows) != 1 || rows[0].OwnerUserID != "" || rows[0].Name != "shared_util" {
		t.Fatalf("unattributed rows = %+v, want the write recorded under owner \"\"", rows)
	}

	// An identified ctx: the same call records THAT identity's UserID.
	alice := server.WithIdentity(context.Background(), &server.Identity{UserID: "u-alice"})
	if _, err := m.PutPymodule(alice, "alice_util", "def alice_util(): pass", "alice's util"); err != nil {
		t.Fatalf("PutPymodule as u-alice: %v", err)
	}
	if rows := f.store.rows["u-alice"]; len(rows) != 1 || rows[0].OwnerUserID != "u-alice" {
		t.Fatalf("u-alice rows = %+v, want the write recorded under owner \"u-alice\"", rows)
	}

	// Listing is owner-scoped the same way: alice's ctx returns only alice's
	// rows, and the anonymous ctx never sees them.
	got, err := m.ListPymodules(alice)
	if err != nil {
		t.Fatalf("ListPymodules as u-alice: %v", err)
	}
	if len(got) != 1 || got[0].Name != "alice_util" {
		t.Fatalf("alice's list = %+v, want only [alice_util]", got)
	}
	anon, err := m.ListPymodules(context.Background())
	if err != nil {
		t.Fatalf("ListPymodules with no identity: %v", err)
	}
	if len(anon) != 1 || anon[0].Name != "shared_util" {
		t.Fatalf("anonymous list = %+v, want only [shared_util]", anon)
	}
}

// TestConnectPyModulesPushesAfterWrite covers push-on-write through the
// adapter: a Put and a Delete each fan SyncPyModules RPCs out to the eligible
// executors, asserted on the real client's recorded requests -- the same
// assertion shape as TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush.
func TestConnectPyModulesPushesAfterWrite(t *testing.T) {
	f := newPymoduleFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}}
	// u_alice (underscore) is the fixture's alice bucket: the pusher resolves
	// exec-alice's owner label through the fixture's username map to exactly
	// that id, so the pushed payloads below carry this Put.
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u_alice"})

	if _, err := m.PutPymodule(ctx, "alice_plot", "def alice_plot(): pass", "plots things"); err != nil {
		t.Fatalf("PutPymodule: %v", err)
	}
	// The put pushed: one SyncPyModules per eligible, resolvable executor
	// (exec-alice and exec-bob; exec-ghost's owner does not resolve).
	if len(f.client.requests) != 2 {
		t.Fatalf("after Put: SyncPyModules requests = %d, want 2", len(f.client.requests))
	}
	if got := moduleNames(f.client.requests[0]); !containsName(got, "alice_plot") {
		t.Errorf("alice's push after Put = %v, want it to contain alice_plot", got)
	}

	if err := m.DeletePymodule(ctx, "alice_plot"); err != nil {
		t.Fatalf("DeletePymodule: %v", err)
	}
	// The delete pushed the pruned corpus too: two more RPCs.
	if len(f.client.requests) != 4 {
		t.Fatalf("after Delete: SyncPyModules requests = %d, want 4", len(f.client.requests))
	}
	if got := moduleNames(f.client.requests[2]); containsName(got, "alice_plot") {
		t.Errorf("alice's push after Delete = %v, want the deleted module gone", got)
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestConnectPyModulesNilPusherStillWrites pins the nil guard: a daemon with
// no executor pool constructs no pusher, and every write must still succeed
// -- the guard is what keeps the write path from panicking, and no RPC can
// have gone out because there is no pusher to make one.
func TestConnectPyModulesNilPusherStillWrites(t *testing.T) {
	f := newPymoduleFixture()
	m := connectPyModules{c: &Controller{pymoduleStore: f.store}} // pymodulePusher deliberately nil
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u-alice"})

	row, err := m.PutPymodule(ctx, "alice_util", "def alice_util(): pass", "alice's util")
	if err != nil {
		t.Fatalf("PutPymodule with nil pusher: %v", err)
	}
	if row.Version != 1 || row.Name != "alice_util" || row.Code != "def alice_util(): pass" {
		t.Errorf("PutPymodule row = %+v, want the saved record echoed back", row)
	}
	if err := m.DeletePymodule(ctx, "alice_util"); err != nil {
		t.Fatalf("DeletePymodule with nil pusher: %v", err)
	}
	if len(f.client.requests) != 0 {
		t.Errorf("SyncPyModules requests = %d, want 0 with no pusher configured", len(f.client.requests))
	}
}
