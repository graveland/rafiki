// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
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
	// Per-executor clients attribute each payload to its executor: pushAll
	// fans out concurrently, so the shared client's arrival order is not
	// deterministic.
	aliceC, bobC := &fakePymoduleClient{}, &fakePymoduleClient{}
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": aliceC,
		"exec-bob":   bobC,
	}
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
	if len(aliceC.requests) != 1 || len(bobC.requests) != 1 {
		t.Fatalf("SyncPyModules requests: exec-alice %d, exec-bob %d; want 1 each (exec-alice and exec-bob)", len(aliceC.requests), len(bobC.requests))
	}
	// exec-alice (owned by alice) received alice's whole corpus, including the
	// module just saved.
	aliceNames := moduleNames(aliceC.requests[0])
	for _, want := range []string{"alice_chart", "alice_plot"} {
		if !slices.Contains(aliceNames, want) {
			t.Errorf("alice's push = %v, want it to contain %q", aliceNames, want)
		}
	}
	// exec-bob (owned by bob) received only bob's corpus: alice's new save
	// must not leak into another owner's push.
	if bobNames := moduleNames(bobC.requests[0]); slices.Contains(bobNames, "alice_plot") || !slices.Contains(bobNames, "bob_util") {
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

// Delete makes the in-memory store behave like the real one: the latest live
// row for (ownerUserID, name) disappears, an unknown or already-deleted name
// reports pymodules.ErrNotFound, and every call is recorded so a test can
// assert which owner a writer bound its delete to. It extends the fake defined
// in pymodulesync_test.go; only the method lives here.
func (s *fakePymoduleStore) Delete(_ context.Context, ownerUserID, name string) error {
	s.deleted = append(s.deleted, [2]string{ownerUserID, name})
	rows := s.rows[ownerUserID]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Name == name {
			s.rows[ownerUserID] = append(rows[:i:i], rows[i+1:]...)
			return nil
		}
	}
	return pymodules.ErrNotFound
}

// TestPymoduleDeleteTriggersOwnerScopedPush covers the delete write path: a
// bound writer's Delete lands in the store under ITS owner, then pushAll fans
// the pruned corpus out, so the next SyncPyModules for alice's executor no
// longer carries the deleted module and bob's payload never did. Assertions
// are on actual SyncPyModules RPC payloads, not on pusher mocks.
func TestPymoduleDeleteTriggersOwnerScopedPush(t *testing.T) {
	f := newPymoduleFixture()
	// Per-executor clients attribute each payload to its executor: pushAll
	// fans out concurrently, so the shared client's arrival order is not
	// deterministic.
	aliceC, bobC := &fakePymoduleClient{}, &fakePymoduleClient{}
	f.pool.clients = map[string]executorpbconnect.ExecutorServiceClient{
		"exec-alice": aliceC,
		"exec-bob":   bobC,
	}
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	if _, err := w.Put(context.Background(), "alice_plot", "def alice_plot(): pass", "plots things"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The put's fan-out reached both eligible executors.
	if len(aliceC.requests) != 1 || len(bobC.requests) != 1 {
		t.Fatalf("after Put, SyncPyModules requests = exec-alice %d, exec-bob %d; want 1 each", len(aliceC.requests), len(bobC.requests))
	}

	if err := w.Delete(context.Background(), "alice_plot"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The delete reached the store under the writer's bound owner.
	if len(f.store.deleted) != 1 || f.store.deleted[0] != [2]string{"u_alice", "alice_plot"} {
		t.Fatalf("store Delete calls = %v, want exactly [{u_alice alice_plot}]", f.store.deleted)
	}
	// The delete pushed the pruned corpus: one more request per eligible executor.
	if len(aliceC.requests) != 2 || len(bobC.requests) != 2 {
		t.Fatalf("after Delete, SyncPyModules requests = exec-alice %d, exec-bob %d; want 2 each", len(aliceC.requests), len(bobC.requests))
	}
	// alice's next payload has dropped the deleted module and keeps the rest.
	if aliceNames := moduleNames(aliceC.requests[1]); slices.Contains(aliceNames, "alice_plot") || !slices.Contains(aliceNames, "alice_chart") {
		t.Errorf("alice's post-delete push = %v, want [alice_chart] without alice_plot", aliceNames)
	}
	// bob's payload (same fan-out) never contained alice's module.
	if bobNames := moduleNames(bobC.requests[1]); slices.Contains(bobNames, "alice_plot") || !slices.Contains(bobNames, "bob_util") {
		t.Errorf("bob's post-delete push = %v, want [bob_util] only", bobNames)
	}
}

// A failed delete must not push: the corpus did not change, so the fake
// client's request count is unchanged from before the delete.
func TestPymoduleDeleteNotFoundSkipsPush(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	before := len(f.client.requests)
	err := w.Delete(context.Background(), "no_such_mod")
	if err == nil {
		t.Fatal("Delete of an unknown name = nil error, want not-found")
	}
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Errorf("Delete error = %v, want it to wrap pymodules.ErrNotFound", err)
	}
	if len(f.client.requests) != before {
		t.Errorf("SyncPyModules requests = %d, want unchanged (%d): a failed delete must not push", len(f.client.requests), before)
	}
}

// TestPymoduleInventoryRendersSavedModules renders the dynamic skill body.
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

// A bound writer's Get reads the latest live row under ITS owner: two
// versions under one name collapse to the higher-id row, and another owner's
// same-named module stays invisible.
func TestControllerPyModuleWriterGet(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: f.pp}

	w := newControllerPyModuleWriter(ctrl, "u_alice")
	if _, err := w.Put(context.Background(), "alice_chart", "x = 1", "v2 of alice's chart"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r, err := w.Get(context.Background(), "alice_chart")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ID != 2 || r.Code != "x = 1" {
		t.Errorf("Get = %+v, want the later version (id 2, code %q)", r, "x = 1")
	}

	// A name owned by bob is invisible to alice's writer: the wrong owner
	// must not leak rows.
	_, err = w.Get(context.Background(), "bob_util")
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Errorf("Get of a bob-owned name = %v, want ErrNotFound", err)
	}
}
