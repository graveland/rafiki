package main

import (
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// A parent confined to an executor must not be able to spawn a child of a kind
// that would run outside the parent's executor set. fundi children honour the
// grant directly (resolveExecutor -> chooseExecutor admits against the
// inherited selector). claude children honour it exactly when the daemon is
// executor-routed: their daraja launches on the pool's machine via
// chooseLaunchExecutor, the same admission pipeline. Without a pool connection
// claudeRunner falls back to a local subprocess on the daemon's own host, so a
// confined agent laundering itself an unconfined sibling is one tool argument
// away — an LLM produces the argument and a prompt injection can dictate it.
// (A third kind, pi, was retired in Phase C0; the generic refusal branch
// covers any kind that might exist without claude's pool-hosted path.)
func TestKindNarrowing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentSel     string
		kind          string
		poolConnected bool
		wantRefused   bool
		wantLocalNote bool // refusal must name the local-subprocess fallback
	}{
		{name: "confined parent, claude child, pool connected", parentSel: "env=ci", kind: protocol.KindClaude, poolConnected: true, wantRefused: false},
		{name: "confined parent, claude child, no pool", parentSel: "env=ci", kind: protocol.KindClaude, wantRefused: true, wantLocalNote: true},
		{name: "confined parent, omitted kind means fundi", parentSel: "env=ci", kind: "", poolConnected: true, wantRefused: false},
		{name: "confined parent, fundi child", parentSel: "env=ci", kind: protocol.KindFundi, wantRefused: false},
		{name: "unconfined parent, claude child, no pool", kind: protocol.KindClaude, wantRefused: false},
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
			}, tc.poolConnected)

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
				if tc.wantLocalNote && !strings.Contains(err.Error(), "local-subprocess fallback") {
					t.Errorf("claude no-pool refusal does not name the local-subprocess fallback: %v", err)
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
// ordinary `rafiki create --kind claude` case and must keep working, with and
// without an executor pool.
func TestKindNarrowingIgnoresTopLevelSpawns(t *testing.T) {
	st := childstore.New()
	for _, poolConnected := range []bool{false, true} {
		if err := checkKindNarrowing(st, protocol.SpawnRequest{Kind: protocol.KindClaude}, poolConnected); err != nil {
			t.Fatalf("a top-level claude spawn was refused (poolConnected=%v): %v", poolConnected, err)
		}
	}
}

// The guard's pool-connected predicate and claudeRunner's fallback predicate
// must be the SAME predicate: the guard may only admit a confined parent's
// claude child when the claude child would actually launch on the pool, and
// claudeRunner may only fall back to the daemon host in exactly the cases the
// guard refused. claudeExecutorRouted is the shared definition; this test
// pins it to the two fields that decide the fallback so the two cannot drift.
func TestClaudeExecutorRoutedMatchesClaudeRunnerFallback(t *testing.T) {
	c := &Controller{}
	if c.claudeExecutorRouted() {
		t.Fatal("no pool: claudeExecutorRouted must be false (claudeRunner falls back locally)")
	}
	c.execPoolConn = &execpool.Pool{}
	if c.claudeExecutorRouted() {
		t.Fatal("execPoolConn alone is not enough: claudeRunner also requires darajaPool")
	}
	c.darajaPool = &darajapool.Pool{}
	if !c.claudeExecutorRouted() {
		t.Fatal("both pool connections set: claude children launch on the pool")
	}
}
