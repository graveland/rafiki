// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

const interruptScript = `
args = ["--effort", "low"]

[[run]]
name  = "first"
turns = ["ready", "run sleep"]
interrupt = "tool_use"

[[run]]
name   = "second"
resume = "first"
turns  = ["recovered"]
args   = ["--max-turns", "3"]
`

func TestParseClaudeScript(t *testing.T) {
	c := assert.NewCollecting(t)
	s, err := parseClaudeScript([]byte(interruptScript))
	c.Require().NoError(err, "a well-formed script parses")
	c.Eq(2, len(s.Runs), "runs")
	c.Eq("tool_use", s.Runs[0].Interrupt, "interrupt")
	c.Eq("first", s.Runs[1].Resume, "resume")
}

func TestParseClaudeScriptRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no runs":         {`args = []`, "no [[run]]"},
		"no turns":        {"[[run]]\nname = \"a\"", "no turns"},
		"unknown key":     {"[[run]]\nturns = [\"x\"]\ninterupt = \"tool_use\"", "unknown key"},
		"bad interrupt":   {"[[run]]\nturns = [\"x\"]\ninterrupt = \"soon\"", "not supported"},
		"resume forward":  {"[[run]]\nturns = [\"x\"]\nresume = \"later\"\n[[run]]\nname = \"later\"\nturns = [\"y\"]", "names no earlier run"},
		"duplicate name":  {"[[run]]\nname = \"a\"\nturns = [\"x\"]\n[[run]]\nname = \"a\"\nturns = [\"y\"]", "used twice"},
		"double resume":   {"[[run]]\nname = \"a\"\nturns = [\"x\"]\n[[run]]\nresume = \"a\"\nturns = [\"y\"]\n[[run]]\nresume = \"a\"\nturns = [\"z\"]", "already resumed"},
		"driver flag":     {"[[run]]\nturns = [\"x\"]\nargs = [\"--resume=abc\"]", "set by the driver"},
		"top driver flag": {"args = [\"-p\"]\n[[run]]\nturns = [\"x\"]", "set by the driver"},
		"bad duration":    {"[[run]]\nturns = [\"x\"]\nturn_timeout = \"soon\"", "parse script"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseClaudeScript([]byte(tc.in))
			c := assert.NewCollecting(t)
			c.Require().Error(err, "must be refused")
			c.StrContains(err.Error(), tc.want, "reason")
		})
	}
}

func TestClaudeScriptPlan(t *testing.T) {
	c := assert.NewCollecting(t)
	s, err := parseClaudeScript([]byte(interruptScript))
	c.Require().NoError(err, "parse")
	plans := s.plan(claudeScriptDefaults{Model: "haiku", SystemPrompt: "sys", Tools: "Bash", Timeout: time.Minute})

	c.Eq(plans[0].SessionID, plans[1].SessionID, "a resumed run keeps its predecessor's session")
	c.True(plans[0].Persist && plans[1].Persist, "both ends of a resume persist")
	c.False(plans[0].Resume, "the first run starts fresh")
	c.True(plans[1].Resume, "the second run resumes")
	c.Eq("haiku", plans[1].Model, "flag default fills in")
	c.EqDeep([]string{"--effort", "low", "--max-turns", "3"}, plans[1].Args, "top-level args first, then the run's")
	c.EqDeep([]string{"--effort", "low"}, plans[0].Args, "top-level args only")
}

func TestClaudeScriptPlanKeepsAnEmptyToolsOverride(t *testing.T) {
	c := assert.NewCollecting(t)
	s, err := parseClaudeScript([]byte("[[run]]\nturns = [\"x\"]\ntools = \"\""))
	c.Require().NoError(err, "parse")
	plans := s.plan(claudeScriptDefaults{Tools: "Bash"})
	c.Eq("", plans[0].Tools, "tools = \"\" means no tools, not the default")
	c.False(plans[0].Persist, "a run in no resume leaves no session file")
}

// TestClaudeTurnsInterruptKillsOnToolUse pins the interrupt: the run ends as
// soon as the last turn's response carries a tool_use block, without waiting
// for the result frame the fake never sends.
func TestClaudeTurnsInterruptKillsOnToolUse(t *testing.T) {
	c := assert.NewCollecting(t)
	bin, _ := fakeClaudeTurns(t, `while IFS= read -r line; do
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}'
  sleep 300
done
`)
	start := time.Now()
	err := runClaudeTurns(claudeTurnsOptions{
		Env:       []string{"PATH=/usr/bin:/bin"},
		Binary:    bin,
		Turns:     []string{"go"},
		Interrupt: claudeInterruptToolUse,
		Timeout:   10 * time.Second,
	})
	c.Require().NoError(err, "an interrupted run is a successful run")
	c.True(time.Since(start) < 8*time.Second, "the run did not wait for a result that never comes")
}

// TestClaudeTurnsInterruptKillsTheToolProcess pins that the kill reaches what
// claude spawned: the fake's background child stands in for a running Bash
// command, and must not outlive the interrupted run.
func TestClaudeTurnsInterruptKillsTheToolProcess(t *testing.T) {
	c := assert.NewCollecting(t)
	pidFile := filepath.Join(t.TempDir(), "tool.pid")
	bin, _ := fakeClaudeTurns(t, `while IFS= read -r line; do
  sleep 300 &
  echo $! > "`+pidFile+`"
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}'
  wait
done
`)
	err := runClaudeTurns(claudeTurnsOptions{
		Env:       []string{"PATH=/usr/bin:/bin"},
		Binary:    bin,
		Turns:     []string{"go"},
		Interrupt: claudeInterruptToolUse,
		Timeout:   10 * time.Second,
	})
	c.Require().NoError(err, "interrupted run")

	raw, err := os.ReadFile(pidFile)
	c.Require().NoError(err, "the fake recorded its tool pid")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	c.Require().NoError(err, "tool pid")
	// SIGKILL delivery is asynchronous; Wait on the group leader already
	// returned, so the child is either gone or a zombie reaped by init.
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		gone = syscall.Kill(pid, 0) != nil
		if !gone {
			time.Sleep(20 * time.Millisecond)
		}
	}
	c.True(gone, "the tool subprocess must not outlive the interrupted run")
}

func TestClaudeTurnsInterruptWithoutToolUseIsAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	bin, _ := fakeClaudeTurns(t, `while IFS= read -r line; do
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
done
`)
	err := runClaudeTurns(claudeTurnsOptions{
		Env:       []string{"PATH=/usr/bin:/bin"},
		Binary:    bin,
		Turns:     []string{"go"},
		Interrupt: claudeInterruptToolUse,
		Timeout:   10 * time.Second,
	})
	c.Require().Error(err, "an interrupt that never happened must not pass")
	c.StrContains(err.Error(), "without a tool call", "reason")
}

// TestScriptedClaudeRunsEveryRunInOrder pins the wiring: one driver call per
// run, the resumed run launched with --resume on its predecessor's session, the
// first with --session-id, and the persistence flag dropped for exactly those.
func TestScriptedClaudeRunsEveryRunInOrder(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	path := filepath.Join(t.TempDir(), "script.toml")
	c.Require().NoError(os.WriteFile(path, []byte(interruptScript), 0o600), "write script")

	orig := runClaudeTurnsFn
	t.Cleanup(func() { runClaudeTurnsFn = orig })
	var got []claudeTurnsOptions
	runClaudeTurnsFn = func(o claudeTurnsOptions) error {
		got = append(got, o)
		return nil
	}

	cmd := newClaudeCmd()
	c.Require().NoError(cmd.ParseFlags([]string{"--url", "http://127.0.0.1:9", "--script", path}), "parse flags")
	c.Require().NoError(runClaude(cmd, nil), "run the script")

	c.Require().Eq(2, len(got), "one driver call per run")
	first, second := strings.Join(got[0].Args, " "), strings.Join(got[1].Args, " ")
	c.StrContains(first, "--session-id ", "the first run names its session")
	c.NotStrContains(first, "--no-session-persistence", "a run that is resumed persists")
	c.StrContains(second, "--resume ", "the second run resumes")
	c.NotStrContains(second, "--no-session-persistence", "a resuming run persists")
	c.Eq(claudeInterruptToolUse, got[0].Interrupt, "interrupt reaches the driver")
	c.Eq("", got[1].Interrupt, "and only the run that asked for it")
	c.StrContains(second, "--effort low --max-turns 3", "script args come last, defaults first")

	sid := first[strings.Index(first, "--session-id ")+len("--session-id "):]
	sid, _, _ = strings.Cut(sid, " ")
	c.StrContains(second, "--resume "+sid, "the resume names the first run's session")
	c.StrContains(strings.Join(got[1].Env, "\n"), "X-Rafiki-Session: "+sid, "both runs land on one conversation")
}

func TestScriptFlagExcludesTurns(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newClaudeCmd()
	c.Require().NoError(cmd.ParseFlags([]string{"--script", "x.toml", "--turn", "hi"}), "parse flags")
	_, err := claudeScriptFromFlags(cmd)
	c.Require().Error(err, "--script with --turn must be refused")
}
