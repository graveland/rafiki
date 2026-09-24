// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestConnectSpawnPrefillMapped pins the wire mapping only: entries arrive on
// SpawnParams as protocol.PrefillRead with path/start/end intact, and a
// request without prefill yields nil — not an empty slice — so the daemon's
// "len == 0 means no pre-fill" checks stay simple. Validation happens in the
// controller (Task 2.2); this layer must not duplicate prefill.Validate's
// rules, so deliberately invalid shapes map through untouched.
func TestConnectSpawnPrefillMapped(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	if _, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work",
		Prefill: []*rafikiv1.PrefillRead{
			{Path: "CLAUDE.md", Start: 10, End: 40},
			{Path: "src/**/*.rs"},
			{Path: "notes.txt", Start: 200},
		},
	})); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	want := []protocol.PrefillRead{
		{Path: "CLAUDE.md", Start: 10, End: 40},
		{Path: "src/**/*.rs"},
		{Path: "notes.txt", Start: 200},
	}
	if !slices.Equal(f.got.Prefill, want) {
		t.Errorf("Prefill = %+v, want %+v", f.got.Prefill, want)
	}
}

func TestConnectSpawnPrefillEmptyStaysNil(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	if _, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"})); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if f.got.Prefill != nil {
		t.Errorf("Prefill = %+v, want nil", f.got.Prefill)
	}
}
