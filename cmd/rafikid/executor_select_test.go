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
	}, "")
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
	}, "")
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
	if _, err := c.selectExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "env=nowhere"}, ""); err == nil {
		t.Fatal("want a refusal")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("selection took %s — it must fail immediately, not wait for an executor", elapsed)
	}
}

func TestSortCandidatesPrefersDurableThenID(t *testing.T) {
	in := []executors.Executor{
		{ID: "zzz-durable"},
		{ID: "bbb-session", Labels: map[string]string{"kind": "session"}},
		{ID: "aaa-durable"},
		{ID: "aaa-session", Labels: map[string]string{"kind": "session"}},
	}
	sortCandidates(in)

	want := []string{"aaa-durable", "zzz-durable", "aaa-session", "bbb-session"}
	for i, w := range want {
		if in[i].ID != w {
			t.Fatalf("position %d: want %s, got %s (full order: %v)", i, w, in[i].ID, ids(in))
		}
	}
}

func TestSortCandidatesIsStableAcrossCalls(t *testing.T) {
	// chooseExecutor returns candidates[0] and Live() ranges a Go map, so
	// without an explicit sort a child lands on an arbitrary executor and a
	// DIFFERENT arbitrary one after a restart.
	first := []executors.Executor{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	second := []executors.Executor{{ID: "c"}, {ID: "b"}, {ID: "a"}}
	sortCandidates(first)
	sortCandidates(second)
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("ordering is not deterministic: %v vs %v", ids(first), ids(second))
		}
	}
}

func ids(in []executors.Executor) []string {
	out := make([]string, len(in))
	for i, e := range in {
		out[i] = e.ID
	}
	return out
}

// The session executor's admits: owner=<user> must actually match a child — the
// whole point of the daemon-attested owner label. Before it existed, every
// spawn was refused because children carried no owner label at all.
func TestAdmissionMatchesDaemonAttestedOwner(t *testing.T) {
	c := selectFixture(t, "", ex("laptop", map[string]string{"kind": "client"}, "owner=brent"))
	c.st.Insert(&childstore.Session{
		ChildID: "c_owned", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind:   protocol.KindFundi,
		Labels: map[string]string{"owner": "brent", "rafiki/kind": "fundi"},
	})
	set, err := c.effectiveExecutorSet("c_owned")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0].ID != "laptop" {
		t.Fatalf("owner=brent admitted the wrong set: %+v", set)
	}
}

// The actual spawn path, not just an already-stored child: a TOP-LEVEL spawn
// (no ParentChildID) has no childstore entry of its own to read an owner
// label back from — Spawn only inserts one after selection succeeds — so
// chooseExecutor must evaluate admission against the owner the caller
// attests for the child about to be created, not an empty label set. Every
// session executor is minted with exactly this admits selector (see
// ExecutorSession), so getting this wrong means `rafiki create` can never
// place a single top-level agent on the operator's own machine.
func TestTopLevelSpawnIsAdmittedByItsAttestedOwner(t *testing.T) {
	c := selectFixture(t, "", ex("laptop", map[string]string{"kind": "client", "owner": "brent"}, "owner=brent"))
	req := protocol.SpawnRequest{ExecutorSelector: "owner=brent,kind=client"}

	chosen, err := c.chooseExecutor(req, "brent")
	if err != nil {
		t.Fatalf("a top-level spawn with its owner attested must reach the laptop executor: %v", err)
	}
	if chosen.ID != "laptop" {
		t.Fatalf("chose %s, want laptop", chosen.ID)
	}

	// And the failure mode this guards against: an unattested (or wrong)
	// owner must still be refused, not silently admitted some other way —
	// proving the fix checks the right thing rather than always succeeding.
	if _, err := c.chooseExecutor(req, ""); err == nil {
		t.Fatal("a top-level spawn with no attested owner reached an owner-scoped executor")
	}
}
