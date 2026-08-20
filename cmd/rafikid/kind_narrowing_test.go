package main

import (
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// A parent confined to an executor must not be able to spawn a child of a kind
// that ignores executors. claude and pi are forked on the daemon's own host and
// resolveExecutor is never consulted for them, so allowing one would let a
// confined agent launder itself an unconfined sibling — by supplying a tool
// argument, which an LLM produces and a prompt injection can dictate.
func TestKindNarrowing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parentSel   string
		kind        string
		wantRefused bool
	}{
		{name: "confined parent, claude child", parentSel: "env=ci", kind: protocol.KindClaude, wantRefused: true},
		{name: "confined parent, pi child", parentSel: "env=ci", kind: protocol.KindPi, wantRefused: true},
		{name: "confined parent, omitted kind means pi", parentSel: "env=ci", kind: "", wantRefused: true},
		{name: "confined parent, fundi child", parentSel: "env=ci", kind: protocol.KindFundi, wantRefused: false},
		{name: "unconfined parent, claude child", kind: protocol.KindClaude, wantRefused: false},
		{name: "unconfined parent, omitted kind", kind: "", wantRefused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := childstore.New()
			st.Insert(&childstore.Session{
				ChildID:          "c_parent",
				ExecutorSelector: tc.parentSel,
			})

			err := checkKindNarrowing(st, protocol.SpawnRequest{
				Kind:          tc.kind,
				ParentChildID: "c_parent",
			})

			if tc.wantRefused {
				if err == nil {
					t.Fatal("spawn was admitted; want a refusal")
				}
				var ce *control.ControllerError
				if !errors.As(err, &ce) {
					t.Fatalf("error is %T, want *control.ControllerError: %v", err, err)
				}
				if ce.Code != protocol.ErrInvalidArgs {
					t.Errorf("code = %q, want %q", ce.Code, protocol.ErrInvalidArgs)
				}
				if !strings.Contains(err.Error(), "executor") {
					t.Errorf("refusal does not explain the executor grant: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("spawn was refused: %v", err)
			}
		})
	}
}

// A top-level spawn has no parent and therefore no grant to widen. This is the
// ordinary `rafiki create --kind claude` case and must keep working.
func TestKindNarrowingIgnoresTopLevelSpawns(t *testing.T) {
	st := childstore.New()
	if err := checkKindNarrowing(st, protocol.SpawnRequest{Kind: protocol.KindClaude}); err != nil {
		t.Fatalf("a top-level claude spawn was refused: %v", err)
	}
}
