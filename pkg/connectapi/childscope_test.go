// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// emptyChildScope is what a per-child credential whose resolve names the
// provenance but NO child must behave as: everything refused. It pins the
// handler branches' fail-closed half against the one shape — ChildID == "" —
// where a bug would fall through to the operator path, most dangerously on
// Spawn, where a forced empty ParentChildID is the TOP-LEVEL spawn shape.
type emptyChildScope struct{}

func (emptyChildScope) ChildID() string { return "" }

func (emptyChildScope) Authorize(target string) error {
	return connect.NewError(connect.CodePermissionDenied, errTestNotDescendant(target))
}

func (emptyChildScope) Subtree([]string) []protocol.ChildSummary { return nil }

func (emptyChildScope) ConversationInScope(string) bool { return false }

func errTestNotDescendant(target string) error {
	return &testDeniedError{target}
}

type testDeniedError struct{ target string }

func (e *testDeniedError) Error() string {
	return "agent " + e.target + " is not a descendant of yours"
}

// emptyScopeServer wires a Server whose child scope always resolves to the
// empty one.
func emptyScopeServer(t *testing.T) *connectapi.Server {
	t.Helper()
	s := connectapi.NewServer(nil)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return emptyChildScope{} })
	s.SetChildLifecycle(emptyLifecycle{})
	return s
}

type emptyLifecycle struct{}

func (emptyLifecycle) Spawn(context.Context, connectapi.SpawnParams) (string, error) {
	return "c_spawned", nil
}

func (emptyLifecycle) Kill(context.Context, string, int64, int64) (connectapi.KillOutcome, error) {
	return connectapi.KillOutcome{}, nil
}

func (emptyLifecycle) Close(context.Context, string) error { return nil }

func (emptyLifecycle) SetBudget(context.Context, string, float64) error { return nil }

// TestSpawnWithEmptyChildIDRefused proves an unnamed-child credential cannot
// spawn — not even top-level, which is what forcing ParentChildID "" would
// mean.
func TestSpawnWithEmptyChildIDRefused(t *testing.T) {
	s := emptyScopeServer(t)
	resp, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Name: "w"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Spawn with an empty-ChildID child scope = %v, want %v", err, connect.CodePermissionDenied)
	}
	if resp != nil {
		t.Fatal("refused spawn produced a response")
	}

	// Even with a parent id on the wire — the child scope overrides it, and
	// the override of an empty id is the refusal above, never a pass-through.
	resp, err = s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Name: "w", ParentChildId: "c_root"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Spawn with parent on the wire = %v, want %v", err, connect.CodePermissionDenied)
	}
	if resp != nil {
		t.Fatal("refused spawn produced a response")
	}
}
