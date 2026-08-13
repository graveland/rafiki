package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// fakePool stands in for *execpool.Pool so selection is testable without a
// listener, a database or a dialling executor.
type fakePool struct{ live []execpool.LiveExecutor }

func (f *fakePool) Live() []execpool.LiveExecutor { return f.live }
func (f *fakePool) ClientFor(id string) (tools.ExecutorClient, error) {
	return &stubExecutorClient{}, nil
}

// stubExecutorClient satisfies tools.ExecutorClient for selection tests, which
// never dispatch a tool call.
type stubExecutorClient struct{}

func (stubExecutorClient) Execute(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}
func (stubExecutorClient) StartJob(context.Context, string) (string, error) { return "", nil }
func (stubExecutorClient) JobOutput(context.Context, string, int64) (tools.JobSnapshot, error) {
	return tools.JobSnapshot{}, nil
}
func (stubExecutorClient) KillJob(context.Context, string) error { return nil }
func (stubExecutorClient) Ping(context.Context) error            { return nil }

func ex(id string, labels map[string]string, admits string) execpool.LiveExecutor {
	return execpool.LiveExecutor{Executor: executors.Executor{
		ID: id, DisplayName: id, Labels: labels, Admits: admits, Enabled: true,
	}}
}

// selectFixture: a coordinator that landed on the `env=home` set, and a child
// beneath it.
func selectFixture(t *testing.T, parentSelector string, live ...execpool.LiveExecutor) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager(), execPool: &fakePool{live: live}}
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: parentSelector, MaxDepth: 1, MaxChildren: 8,
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi,
		Labels: map[string]string{
			childstore.LabelParent: "c_parent", childstore.LabelRoot: "c_parent",
			"rafiki/kind": "fundi",
		},
	})
	return c
}

// THE property. A child asking for something outside its parent's set gets
// nothing — not because its selector was proved to imply its parent's (that is
// a logic puzzle the moment notin appears, and it fails OPEN), but because the
// sets are intersected.
func TestChildCannotReachAnExecutorItsParentCouldNot(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	_, err := c.selectExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=work",
	})
	if err == nil {
		t.Fatal("a child reached outside its parent's effective set")
	}
	if !strings.Contains(err.Error(), "parent") && !strings.Contains(err.Error(), "PARENT") {
		t.Errorf("the refusal must say the parent's set is why: %v", err)
	}
}

func TestChildInheritsTheParentsSetWhenItNamesNoSelector(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	set, err := c.effectiveExecutorSet("c_child")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0].ID != "exec-home" {
		t.Fatalf("want only exec-home, got %+v", set)
	}
}

// Narrowing is transitive: a grandchild is bounded by its grandparent.
func TestNarrowingIsTransitiveUpTheWholeChain(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("a", map[string]string{"env": "home", "os": "linux"}, ""),
		ex("b", map[string]string{"env": "home", "os": "darwin"}, ""),
		ex("z", map[string]string{"env": "work", "os": "linux"}, ""),
	)
	_ = c.st.Update("c_child", func(s *childstore.Session) { s.ExecutorSelector = "os=linux" })
	c.st.Insert(&childstore.Session{
		ChildID: "c_grandchild", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_child", childstore.LabelRoot: "c_parent",
		},
	})

	set, err := c.effectiveExecutorSet("c_grandchild")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0].ID != "a" {
		t.Fatalf("grandchild must be bounded by env=home AND os=linux; got %+v", set)
	}
}

// A top-level agent's set is everything live, subject to executor admission.
func TestTopLevelAgentSeesEveryAdmittingExecutor(t *testing.T) {
	c := selectFixture(t, "",
		ex("a", map[string]string{"env": "home"}, ""),
		ex("b", map[string]string{"env": "work"}, ""),
	)
	set, err := c.effectiveExecutorSet("c_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 {
		t.Fatalf("want both, got %+v", set)
	}
}

// Scheduling failure is fast and legible. The current message says "labels do
// not match selector" for every candidate regardless of the real reason, which
// is a diagnostic that cannot distinguish a typo from a missing label from an
// executor that refused the child.
func TestNoMatchNamesTheExcludingPredicatePerExecutor(t *testing.T) {
	c := selectFixture(t, "",
		ex("exec-home", map[string]string{"env": "home", "os": "linux"}, ""),
		ex("exec-mac", map[string]string{"env": "work", "os": "darwin"}, ""),
		ex("exec-picky", map[string]string{"env": "work", "os": "linux"}, "rafiki/kind=claude"),
	)
	_, err := c.selectExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=work,os=linux",
	})
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		"env=work,os=linux",     // what was required
		"exec-home", "env=home", // wrong label value, named
		"exec-mac", "os=darwin",
		"exec-picky", "admission", "rafiki/kind", // the executor refused the child
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
}

// Queueing is opt-in only. A spawn that matches nothing fails NOW — silent
// queueing turns a structural mistake into a hang nobody can diagnose.
func TestNoMatchFailsImmediatelyRatherThanQueueing(t *testing.T) {
	c := selectFixture(t, "")
	start := time.Now()
	if _, err := c.selectExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "env=nowhere"}); err == nil {
		t.Fatal("want a refusal")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("selection took %s — it must fail immediately, not wait for an executor", elapsed)
	}
}
