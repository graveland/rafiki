// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestMergeEnvOverlayWins covers the per-child environment join: every key in
// the overlay replaces the base's entry, keys only in the base pass through.
// This is the precedence ToolOpts.Env is built with in BuildRuntime (process
// env as base, forwarded caller env as overlay) and the reason a caller's
// forwarded value reaches exactly its own child's subprocesses and nobody
// else's.
func TestMergeEnvOverlayWins(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "HOME=/home/op", "KEEP=base"}
	overlay := []string{"PATH=/opt/pinned/bin", "EXTRA=1"}

	got := MergeEnv(base, overlay)
	env := envOf(got)

	c := assert.NewAborting(t)
	c.Eq("/opt/pinned/bin", env["PATH"], "overlay must replace the base's PATH")
	c.Eq("/home/op", env["HOME"], "base-only keys pass through")
	c.Eq("base", env["KEEP"], "base-only keys pass through")
	c.Eq("1", env["EXTRA"], "overlay-only keys land")
	// Exactly one PATH entry: exec.Cmd dedup keeps the last duplicate, so a
	// stale base copy shadowing (or shadowed by) the overlay would make the
	// winner depend on append order rather than on MergeEnv's contract.
	c.Eq(1, countKey(got, "PATH"), "PATH must appear exactly once")
}

// TestMergeEnvNoOverlayIsBase pins that an empty overlay is the identity: a
// caller that forwarded nothing must not perturb the pinned environment at
// all.
func TestMergeEnvNoOverlayIsBase(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin"}
	got := MergeEnv(base, nil)
	assert.NewAborting(t).True(len(got) == 1 && got[0] == base[0], "no overlay = base unchanged, got %v", got)
}

// TestEnvGet covers the environ-slice lookup pymodule_run's Materialize-time
// interpreter resolution and PYTHONPATH folding use.
func TestEnvGet(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin", "EMPTY=", "WEIRD=a=b"}
	c := assert.NewAborting(t)
	c.Eq("/usr/bin:/bin", EnvGet(env, "PATH"), "value after the first =")
	c.Eq("", EnvGet(env, "EMPTY"), "empty value reads as empty")
	c.Eq("a=b", EnvGet(env, "WEIRD"), "value may contain =")
	c.Eq("", EnvGet(env, "MISSING"), "absent key")
}

// TestBashRunsUnderPinnedEnv is the drift-immunity pin: the bash child runs
// under ToolOpts.Env EXACTLY, so a later mutation of the serving process's
// own environment — however it happens — cannot reach the child. This is the
// failure the pinning exists to prevent: an executor serving children for
// hours was observed with its process environment drifting after startup,
// and every child spawned after the drift inherited the drifted PATH.
func TestBashRunsUnderPinnedEnv(t *testing.T) {
	// Materialize FIRST — the executor builds its registries at startup,
	// when its environment files have just been applied — and drift the
	// process environment afterwards, as whatever mutates a long-lived
	// process's environment does.
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		Cwd: t.TempDir(),
		Env: []string{
			"PATH=/usr/bin:/bin",
			"PINNED_PROBE=pinned",
		},
	})
	assert.NewAborting(t).NoError(err)
	r := NewRegistry()
	r.Register(tool)

	// Now the drift: after the registry exists, before the first spawn.
	t.Setenv("PATH", "/drifted/bin")
	t.Setenv("PINNED_PROBE", "drifted")

	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"printf '%s|%s' \"$PINNED_PROBE\" \"${PATH%%:*}\""}`))
	assert.NewAborting(t).NoError(err)
	c := assert.NewAborting(t)
	c.StrContains(out, "pinned|/usr/bin", "child must see the pinned env, got")
	c.NotStrContains(out, "drifted", "child must not see the drifted process env, got")
}

// TestBashNilEnvInheritsProcess pins the zero-value contract: a bare
// ToolOpts{} (every test constructing one) keeps the pre-threading behavior —
// the child inherits the process environment, including a value set AFTER the
// tool was materialized.
func TestBashNilEnvInheritsProcess(t *testing.T) {
	t.Setenv("PINNED_PROBE", "process")
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{Cwd: t.TempDir()})
	assert.NewAborting(t).NoError(err)
	r := NewRegistry()
	r.Register(tool)
	out, execErr := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"printenv PINNED_PROBE"}`))
	assert.NewAborting(t).NoError(execErr)
	assert.NewAborting(t).Eq("process", strings.TrimSpace(out), "nil env = process environment")
}

// envOf parses an environ slice into a map, last duplicate wins (exec.Cmd's
// rule).
func envOf(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			n++
		}
	}
	return n
}

// TestAgentEnvMarkersWinAndBaseSurvives pins AgentEnv: the agent markers
// override any inherited or forwarded value, everything else passes through,
// a nil base means the process environment (never an env of only the
// markers, which would strip PATH), and an empty child id sets no
// RAFIKI_CHILD_ID at all.
func TestAgentEnvMarkersWinAndBaseSurvives(t *testing.T) {
	c := assert.NewCollecting(t)
	got := AgentEnv([]string{"PATH=/p", "AI_AGENT=claude-code", "RAFIKI_CHILD_ID=other"}, "c_me")
	env := envOf(got)
	c.Eq("rafiki", env["AI_AGENT"], "AI_AGENT")
	c.Eq("c_me", env["RAFIKI_CHILD_ID"], "RAFIKI_CHILD_ID")
	c.Eq("/p", env["PATH"], "base passes through")
	c.Eq(1, countKey(got, "AI_AGENT"), "AI_AGENT exactly once")

	t.Setenv("AGENTENV_PROBE", "from-process")
	c.Eq("from-process", envOf(AgentEnv(nil, "c_me"))["AGENTENV_PROBE"], "nil base = process environment")

	_, has := envOf(AgentEnv([]string{"PATH=/p"}, ""))["RAFIKI_CHILD_ID"]
	c.False(has, "empty child id must not set RAFIKI_CHILD_ID")
}
