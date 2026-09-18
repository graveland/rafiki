// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestMCPPyModuleStoreAdapter(t *testing.T) {
	ctx := context.Background()
	store := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	ctrl := &Controller{pymoduleStore: store, pymodulePusher: nil}

	// Test Put with nil pusher
	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u-alice"})
	id, err := mcp.Put(ctx, "helper", "def f(): pass", "a helper")
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if id != 1 {
		t.Errorf("Put returned id %d, want 1", id)
	}

	// Verify record is stored
	if records := store.rows["u-alice"]; len(records) != 1 {
		t.Errorf("fakePymoduleStore.rows[u-alice] has %d record(s), want 1", len(records))
	} else if records[0].Name != "helper" {
		t.Errorf("record Name is %q, want %q", records[0].Name, "helper")
	}

	// Test Delete with nil pusher
	if err := mcp.Delete(ctx, "helper"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deletion is tracked
	if len(store.deleted) != 1 {
		t.Errorf("fakePymoduleStore.deleted has %d entry/entries, want 1", len(store.deleted))
	} else if store.deleted[0] != [2]string{"u-alice", "helper"} {
		t.Errorf("deleted entry is %v, want [u-alice helper]", store.deleted[0])
	}
}

// The MCP face's Get is bound to its constructed owner: it serves that
// owner's latest live row and cannot see another owner's same-named module.
func TestMCPPyModuleStoreGet(t *testing.T) {
	f := newPymoduleFixture()
	ctrl := &Controller{pymoduleStore: f.store, pymodulePusher: nil}

	mcp := newMCPPyModuleStore(ctrl, users.Identity{UserID: "u_alice"})
	r, err := mcp.Get(context.Background(), "alice_chart")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ID != 1 || r.Name != "alice_chart" || r.Code != "def alice_chart(): pass" {
		t.Errorf("Get = %+v, want alice's row (id 1)", r)
	}

	_, err = mcp.Get(context.Background(), "bob_util")
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Errorf("Get of a bob-owned name = %v, want ErrNotFound", err)
	}
}
