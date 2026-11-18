package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
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
			assert.NewAborting(t).False(got.ExecutorRef != tt.wantRef || got.ExecutorSelector != tt.wantSelector, "promoteBareExecutorRef(ref=%q, selector=%q) = (ref=%q, selector=%q), want (ref=%q, selector=%q)", tt.ref, tt.selector, got.ExecutorRef, got.ExecutorSelector, tt.wantRef, tt.wantSelector)
		})
	}
}

// THE fix-2 regression: on a daemon with an executor pool, an agent spawned
// with NO selector — an MCP caller's omission — still gets a boundExecutor.
// This is the exact shape of the incident's glm-test spawn, which started,
// ran fine, and had the whole workspace tier missing with no error anywhere.
func TestEmptySelectorOnAPoolDaemonStillGetsABoundExecutor(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{live: []execpool.LiveExecutor{
		ex("exec-1", map[string]string{"env": "home"}, ""),
	}}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	parentID := seedChild(t, c)
	req := baseRequest()
	req.ParentChildID = parentID // parented: tolerates the fake pool's failed provision
	ro, err := c.spawnAgentRuntimeOptions(req, "c1", false, "brent", "")
	ck.NoError(err, "an empty selector on a pool daemon must not mean no executor")
	ck.NotNil(ro.Executor, "Executor is nil: MaterializeAll will drop the whole workspace tier and the child silently runs without filesystem tools")
	// hasExecutor is threaded from the pool, not the selector, so the
	// cwd-local skill dirs are dropped exactly when an executor may serve the
	// project tier — same predicate, or an unbound child's skills disagree
	// with its bound sibling's.
	ck.NotContains(ro.SkillsDirs, "/tmp/.claude/skills", "SkillsDirs")
}

// And the loud counterpart: with the pool configured but nothing live, a
// top-level spawn with no selector STARTS UNBOUND — its workspace tools are
// present and fail with the diagnostic until an executor connects — and
// explainNoMatch's per-candidate text rides back as a spawn warning instead of
// being swallowed.
func TestTopLevelSpawnWithNoSelectorAndNoLiveExecutorStartsUnbound(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	req := baseRequest() // top-level, no selector
	var warnings []string
	ro, err := c.agentRuntimeOptionsFor(req, "c1", false, "brent", "", "", "", &warnings)
	ck.NoError(err, "a top-level spawn with no executor available must start unbound, not be refused")
	ck.NotNil(ro.Executor, "Executor must still be non-nil, or MaterializeAll drops the "+
		"whole workspace tier and the child silently runs tools in the daemon")
	ck.Require().Len(warnings, 1, "warnings = %v, want exactly the unbound diagnostic", warnings)
	ck.StrContains(warnings[0], "no executor available yet; the child starts unbound: ", "warning prefix: %q", warnings[0])
	ck.StrContains(warnings[0], "live executor(s)", "the warning must carry explainNoMatch's diagnostic: %v", warnings[0])
	wl, ok := c.takeWorkspaceLabels("c1")
	ck.True(ok, "the child must be marked unbound for Spawn's label merge")
	ck.Eq("unbound", wl.executorState, "executorState")
}
