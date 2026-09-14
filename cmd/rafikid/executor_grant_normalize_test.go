package main

import (
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// The MCP incident this normalization exists for: an external service passed
// executor: "greyshift", ParseSelector read the bare word as "must carry a
// label named greyshift" (a guaranteed zero match against labels {owner,
// machine}), and the spawn was refused while the SAME word works on the CLI,
// whose --executor is a ref. A bare word is now promoted to a ref at spawn.
func TestPromoteBareExecutorRef(t *testing.T) {
	tests := []struct {
		name          string
		ref, selector string
		wantRef       string
		wantSelector  string
	}{
		{"bare machine name becomes a ref", "", "greyshift", "greyshift", ""},
		{"selector syntax is untouched", "", "env=work", "", "env=work"},
		{"compound selector is untouched", "", "env=work,os=linux", "", "env=work,os=linux"},
		{"absence term is untouched", "", "!os", "", "!os"},
		{"set membership is untouched", "", "os in (linux,darwin)", "", "os in (linux,darwin)"},
		{"comma-joined keys are untouched", "", "env,os", "", "env,os"},
		{"an explicit ref wins and the selector stands", "exec-1", "greyshift", "exec-1", "greyshift"},
		{"empty request is untouched", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := promoteBareExecutorRef(protocol.SpawnRequest{
				ExecutorRef:      tt.ref,
				ExecutorSelector: tt.selector,
			})
			if got.ExecutorRef != tt.wantRef || got.ExecutorSelector != tt.wantSelector {
				t.Fatalf("promoteBareExecutorRef(ref=%q, selector=%q) = (ref=%q, selector=%q), want (ref=%q, selector=%q)",
					tt.ref, tt.selector, got.ExecutorRef, got.ExecutorSelector, tt.wantRef, tt.wantSelector)
			}
		})
	}
}

// refFixture: one top-level controller whose pool holds the given executors.
// selectFixture also seeds c_parent/c_child; the ref tests below use the
// parent when they need a constrained lineage.
func refFixture(t *testing.T, live ...execpool.LiveExecutor) *Controller {
	t.Helper()
	return selectFixture(t, "", live...)
}

// The fold's whole point: a ref-only grant must reach the stored session as a
// selector, or lineage narrowing sees "" and the child's subtree escapes the
// pin (and resume rebuilds without it). machine=<label> admits exactly the
// resolved row among the executors the child may reach — machine labels are
// unique per owner (executors_owner_machine_unique).
func TestPersistRefAsSelectorFoldsAMachineName(t *testing.T) {
	c := refFixture(t, ex("exec-1", map[string]string{"machine": "greyshift", "owner": "brent"}, ""))
	got, err := c.persistRefAsSelector(protocol.SpawnRequest{ExecutorRef: "greyshift"}, "brent")
	if err != nil {
		t.Fatalf("persistRefAsSelector: %v", err)
	}
	if got.ExecutorSelector != "machine=greyshift" {
		t.Fatalf("selector = %q, want machine=greyshift", got.ExecutorSelector)
	}
	if got.ExecutorRef != "" {
		t.Fatalf("ref = %q, want it cleared once the selector carries the pin", got.ExecutorRef)
	}

	// The folded selector must resolve to the same row the ref did, so the
	// binder's later re-selection (ChooseFor re-runs chooseExecutor) lands on
	// the same machine instead of re-running the ref search.
	chosen, err := c.chooseExecutor(protocol.SpawnRequest{ExecutorSelector: got.ExecutorSelector}, "brent")
	if err != nil {
		t.Fatalf("the folded selector must resolve: %v", err)
	}
	if chosen.ID != "exec-1" {
		t.Fatalf("folded selector chose %s, want exec-1", chosen.ID)
	}
}

// matchExecutorRef falls back to the raw id when no machine label matches;
// the fold must handle that spelling too.
func TestPersistRefAsSelectorFoldsAnExecutorID(t *testing.T) {
	c := refFixture(t, ex("exec-1", map[string]string{"machine": "greyshift"}, ""))
	got, err := c.persistRefAsSelector(protocol.SpawnRequest{ExecutorRef: "exec-1"}, "brent")
	if err != nil {
		t.Fatalf("persistRefAsSelector: %v", err)
	}
	if got.ExecutorSelector != "machine=greyshift" || got.ExecutorRef != "" {
		t.Fatalf("got (ref=%q, selector=%q), want (ref=\"\", selector=\"machine=greyshift\")",
			got.ExecutorRef, got.ExecutorSelector)
	}
}

// A row without a machine label cannot be expressed as a selector (selectors
// match labels, never ids), so the ref survives: the child still binds through
// it this generation, with claude's inherently-pinned posture for its row.
func TestPersistRefAsSelectorKeepsTheRefWithoutAMachineLabel(t *testing.T) {
	c := refFixture(t, ex("exec-2", map[string]string{"env": "x"}, ""))
	got, err := c.persistRefAsSelector(protocol.SpawnRequest{ExecutorRef: "exec-2"}, "brent")
	if err != nil {
		t.Fatalf("persistRefAsSelector: %v", err)
	}
	if got.ExecutorRef != "exec-2" || got.ExecutorSelector != "" {
		t.Fatalf("got (ref=%q, selector=%q), want the ref kept as-is", got.ExecutorRef, got.ExecutorSelector)
	}
}

// resolveRef runs on the confinement-narrowed eligible set, never the raw
// pool — a ref to a machine the caller cannot reach is refused, exactly like
// a selector naming one.
func TestPersistRefAsSelectorRefusesAMachineOutsideTheParentsSet(t *testing.T) {
	c := selectFixture(t, "env=home",
		ex("exec-home", map[string]string{"env": "home", "machine": "homeshift"}, ""),
		ex("exec-work", map[string]string{"env": "work", "machine": "workbox"}, ""),
	)
	_, err := c.persistRefAsSelector(protocol.SpawnRequest{
		ParentChildID: "c_parent",
		ExecutorRef:   "workbox",
	}, "brent")
	if err == nil {
		t.Fatal("a ref reached a machine the parent's set excludes")
	}
	if !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("the refusal must say why the named machine is unusable: %v", err)
	}
}

func TestPersistRefAsSelectorNamesAMissingMachine(t *testing.T) {
	c := refFixture(t, ex("exec-1", map[string]string{"machine": "greyshift"}, ""))
	_, err := c.persistRefAsSelector(protocol.SpawnRequest{ExecutorRef: "silvershift"}, "brent")
	if err == nil {
		t.Fatal("want a refusal for an unknown machine name")
	}
	if !strings.Contains(err.Error(), `no executor named "silvershift"`) {
		t.Fatalf("the refusal must name the missing machine: %v", err)
	}
}

// No pool: the fold cannot resolve anything and must not invent a refusal —
// a pool-less daemon's ref-only grant keeps today's toolless in-process
// posture (the runtime gate is keyed off the pool, which is nil here).
func TestPersistRefAsSelectorIsSkippedWithoutAPool(t *testing.T) {
	c := refFixture(t)
	c.execPool = nil
	req := protocol.SpawnRequest{ExecutorRef: "greyshift"}
	got, err := c.persistRefAsSelector(req, "brent")
	if err != nil {
		t.Fatalf("persistRefAsSelector without a pool must be a no-op, got %v", err)
	}
	if got.ExecutorRef != "greyshift" || got.ExecutorSelector != "" {
		t.Fatalf("request was mutated without a pool: (ref=%q, selector=%q)", got.ExecutorRef, got.ExecutorSelector)
	}
}

// Selector-and-ref together: the daemon resolves by ref first (types.go's
// documented precedence) and the fold must not clobber the caller's selector.
func TestPersistRefAsSelectorSkipsWhenASelectorIsAlreadyPresent(t *testing.T) {
	c := refFixture(t, ex("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, ""))
	req := protocol.SpawnRequest{ExecutorRef: "greyshift", ExecutorSelector: "env=home"}
	got, err := c.persistRefAsSelector(req, "brent")
	if err != nil {
		t.Fatalf("persistRefAsSelector: %v", err)
	}
	if got.ExecutorSelector != "env=home" || got.ExecutorRef != "greyshift" {
		t.Fatalf("got (ref=%q, selector=%q), want both left alone", got.ExecutorRef, got.ExecutorSelector)
	}
}

// THE fix-2 regression: on a daemon with an executor pool, an agent spawned
// with NO selector — an MCP caller's omission — still gets a boundExecutor.
// This is the exact shape of the incident's glm-test spawn, which started,
// ran fine, and had the whole workspace tier missing with no error anywhere.
func TestEmptySelectorOnAPoolDaemonStillGetsABoundExecutor(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{live: []execpool.LiveExecutor{
		ex("exec-1", map[string]string{"env": "home"}, ""),
	}}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	parentID := seedChild(t, c)
	req := baseRequest()
	req.ParentChildID = parentID // parented: tolerates the fake pool's failed provision
	ro, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err != nil {
		t.Fatalf("an empty selector on a pool daemon must not mean no executor: %v", err)
	}
	if ro.Executor == nil {
		t.Fatal("Executor is nil: MaterializeAll will drop the whole workspace tier and the child silently runs without filesystem tools")
	}
	// hasExecutor is threaded from the pool, not the selector, so the
	// cwd-local skill dirs are dropped exactly when an executor may serve the
	// project tier — same predicate, or an unbound child's skills disagree
	// with its bound sibling's.
	if slices.Contains(ro.SkillsDirs, "/tmp/.claude/skills") {
		t.Fatalf("SkillsDirs = %v, want no cwd-local dirs on a pool daemon", ro.SkillsDirs)
	}
}

// And the loud counterpart: with the pool configured but nothing live, a
// top-level spawn with no selector is REFUSED with explainNoMatch's
// diagnostic instead of silently starting a toolless agent.
func TestTopLevelSpawnWithNoSelectorAndNoLiveExecutorIsRefused(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	req := baseRequest() // top-level, no selector
	_, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err == nil {
		t.Fatal("a top-level spawn with no executor available must be refused, not started toolless")
	}
	if !strings.Contains(err.Error(), "live executor") {
		t.Fatalf("the refusal must carry explainNoMatch's diagnostic: %v", err)
	}
}
