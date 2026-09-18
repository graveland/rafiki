// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush covers the whole
// write path: a bound writer's Put lands in the store under ITS owner, then
// pushAll fans the new corpus out to the owner's executors -- and to nobody
// else's. The fake pusher is the real pymodulePusher over the fake pool and
// store fixture from pymodulesync_test.go, so the assertion is on actual
// SyncPyModules RPCs, not on a mock of the push.
func TestNewControllerPyModuleWriterPutTriggersOwnerScopedPush(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	id, err := w.Put(context.Background(), "alice_plot", "def alice_plot(): pass", "plots things")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != 2 { // alice's fake store already held one row (ID 1)
		t.Errorf("Put id = %d, want 2", id)
	}

	// The put triggered a push: one SyncPyModules RPC per eligible executor.
	if len(f.client.requests) != 2 {
		t.Fatalf("SyncPyModules requests = %d, want 2 (exec-alice and exec-bob)", len(f.client.requests))
	}
	// exec-alice (owned by alice) received alice's whole corpus, including the
	// module just saved.
	aliceNames := moduleNames(f.client.requests[0])
	for _, want := range []string{"alice_chart", "alice_plot"} {
		if !slices.Contains(aliceNames, want) {
			t.Errorf("alice's push = %v, want it to contain %q", aliceNames, want)
		}
	}
	// exec-bob (owned by bob) received only bob's corpus: alice's new save
	// must not leak into another owner's push.
	if bobNames := moduleNames(f.client.requests[1]); slices.Contains(bobNames, "alice_plot") || !slices.Contains(bobNames, "bob_util") {
		t.Errorf("bob's push = %v, want [bob_util] only", bobNames)
	}
}

// A daemon with no executor pool constructs no pusher; a Put must still save
// (the child can list its own modules even though nothing syncs anywhere).
func TestNewControllerPyModuleWriterPutWithNilPusherStillSaves(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store} // pymodulePusher deliberately nil

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	id, err := w.Put(context.Background(), "alice_plot", "def alice_plot(): pass", "plots things")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != 2 {
		t.Errorf("Put id = %d, want 2", id)
	}
	if len(f.client.requests) != 0 {
		t.Errorf("SyncPyModules requests = %d, want 0 with no pusher configured", len(f.client.requests))
	}
}

// An anonymous spawn's empty owner is passed through to the store unchanged:
// pymodules.Store treats it as the shared unattributed bucket, never global.
func TestNewControllerPyModuleWriterPassesEmptyOwnerThrough(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store}

	w := newControllerPyModuleWriter(ctrl, "")
	id, err := w.Put(context.Background(), "unattributed_util", "x = 1", "nobody's util")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != 1 {
		t.Errorf("Put id = %d, want 1", id)
	}
	rows := f.store.rows[""]
	if len(rows) != 1 || rows[0].OwnerUserID != "" || rows[0].Name != "unattributed_util" {
		t.Errorf("unattributed rows = %+v, want the save recorded under the empty owner", rows)
	}
}

func TestPymoduleInventoryRendersSavedModules(t *testing.T) {
	// A fresh store, not the shared fixture: the fixture pre-seeds rows, and
	// the empty-store case needs an owner with nothing saved.
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	ctrl := &Controller{pymoduleStore: store}

	// Empty store: a clear hint, not an empty string.
	body, err := pymoduleInventory(ctrl, "u_alice")(context.Background())
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if body != "No pymodules saved yet. Use pymodule_put to save one." {
		t.Errorf("empty-store inventory = %q, want the nothing-saved hint", body)
	}

	if _, err := newControllerPyModuleWriter(ctrl, "u_alice").Put(context.Background(), "alice_chart", "x = 1", "charts things"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, err = pymoduleInventory(ctrl, "u_alice")(context.Background())
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if body != "alice_chart — charts things\n" {
		t.Errorf("inventory = %q, want the saved module's name and description", body)
	}
}
