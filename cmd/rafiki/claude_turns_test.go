// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/profile"
)

// fakeClaudeTurns writes a stand-in for the claude binary that speaks the
// stream-json contract this driver relies on: every stdin line is logged, then
// one assistant frame and one result frame are emitted for it.
func fakeClaudeTurns(t *testing.T, body string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	log = filepath.Join(dir, "turns.log")
	script := "#!/bin/sh\nLOG=\"" + log + "\"\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return bin, log
}

// TestClaudeTurnsDrivesEveryTurn pins the point of the driver: N prompts become
// N stream-json user messages in ONE process, each awaited on its result frame.
func TestClaudeTurnsDrivesEveryTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	bin, log := fakeClaudeTurns(t, `while IFS= read -r line; do
  printf '%s\n' "$line" >> "$LOG"
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"reply"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
done
`)

	var out bytes.Buffer
	err := runClaudeTurns(claudeTurnsOptions{
		Env:     []string{"PATH=/usr/bin:/bin"},
		Binary:  bin,
		Turns:   []string{"first", "second", "third"},
		Out:     &out,
		Timeout: 10 * time.Second,
	})
	c.Require().NoError(err, "a scripted session that answers every turn must succeed")

	sent, err := os.ReadFile(log)
	c.Require().NoError(err, "read the fake's log")
	lines := strings.Split(strings.TrimSpace(string(sent)), "\n")
	c.Eq(3, len(lines), "one stdin frame per turn, in one process")
	for _, line := range lines {
		c.StrContains(line, `"type":"user"`, "the provider must encode a stream-json user message")
	}
	c.StrContains(lines[0], "first", "turns go out in order")
	c.StrContains(lines[2], "third", "turns go out in order")
	c.StrContains(out.String(), "reply", "assistant text is echoed while the session runs")
}

// TestClaudeTurnsFailsOnErrorResult pins that a turn claude reports as failed is
// an error here too, rather than a silent success that leaves a truncated
// capture looking complete.
func TestClaudeTurnsFailsOnErrorResult(t *testing.T) {
	c := assert.NewCollecting(t)
	bin, _ := fakeClaudeTurns(t, `while IFS= read -r line; do
  printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"model refused"}'
done
`)

	err := runClaudeTurns(claudeTurnsOptions{
		Env:     []string{"PATH=/usr/bin:/bin"},
		Binary:  bin,
		Turns:   []string{"go"},
		Timeout: 10 * time.Second,
	})
	c.Require().Error(err, "an error result must fail the run")
	c.StrContains(err.Error(), "model refused", "the upstream's own message must survive")
}

// TestClaudeTurnsTimesOutOnASilentSession pins the wedge case: a session that
// never answers must fail within the per-turn ceiling instead of hanging.
func TestClaudeTurnsTimesOutOnASilentSession(t *testing.T) {
	c := assert.NewCollecting(t)
	bin, _ := fakeClaudeTurns(t, `read -r line
sleep 10
`)

	start := time.Now()
	err := runClaudeTurns(claudeTurnsOptions{
		Env:     []string{"PATH=/usr/bin:/bin"},
		Binary:  bin,
		Turns:   []string{"go"},
		Timeout: 150 * time.Millisecond,
	})
	c.Require().Error(err, "a silent session must fail")
	c.StrContains(err.Error(), "no result frame", "the timeout must name what it waited for")
	c.True(time.Since(start) < 5*time.Second, "the timeout must fire promptly, not after the script's sleep")
}

// TestClaudeTurnsRejectsAnEmptySession keeps the flag contract honest: a
// scripted run with nothing to say is a usage error, not a no-op process.
func TestClaudeTurnsRejectsAnEmptySession(t *testing.T) {
	c := assert.NewCollecting(t)
	err := runClaudeTurns(claudeTurnsOptions{Binary: "claude"})
	c.Require().Error(err, "no turns must be refused")
	c.StrContains(err.Error(), "--turn", "the error must name the flags that supply turns")
}

// TestClaudeTurnsFromFlags pins the two prompt sources and their order: --turn
// first, then the file's lines, with blanks and comments skipped.
func TestClaudeTurnsFromFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "turns.txt")
	body := "# a comment\n\nfirst from file\nsecond from file\n"
	c.Require().NoError(os.WriteFile(file, []byte(body), 0o600), "write turn file")

	cmd := newClaudeCmd()
	cmd.SetArgs([]string{"--turn", "inline one", "--turn", "inline two", "--turn-file", file})
	c.Require().NoError(cmd.ParseFlags([]string{"--turn", "inline one", "--turn", "inline two", "--turn-file", file}), "parse flags")

	turns, err := claudeTurnsFromFlags(cmd)
	c.Require().NoError(err, "claudeTurnsFromFlags")
	c.EqDeep([]string{"inline one", "inline two", "first from file", "second from file"}, turns,
		"--turn comes first, then the file, with comments and blanks skipped")
}

// TestClaudeTurnsFlagNeedsPrompts pins that the explicit switch cannot be used
// to launch an empty session.
func TestClaudeTurnsFlagNeedsPrompts(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newClaudeCmd()
	c.Require().NoError(cmd.ParseFlags([]string{"--turns"}), "parse flags")

	_, err := claudeTurnsFromFlags(cmd)
	c.Require().Error(err, "--turns with no prompts must be refused")
	c.StrContains(err.Error(), "--turn", "the error must say how to supply them")
}

// TestResolveClaudeURLPinsScriptedPrecedence is the regression this wiring hit
// for real: a scripted run inside `rafikid proxy record --exec` inherited the
// recorder's ANTHROPIC_BASE_URL, but the profile's proxy won, so every request
// went to the daemon instead of the recorder and the capture stayed empty.
func TestResolveClaudeURLPinsScriptedPrecedence(t *testing.T) {
	c := assert.NewCollecting(t)
	const (
		flag    = "http://flag.example"
		fromEnv = "http://127.0.0.1:65029"
		profile = "https://rafiki.example"
	)
	c.Eq(flag, resolveClaudeURL(flag, true, fromEnv, profile), "--url always wins")
	c.Eq(fromEnv, resolveClaudeURL("", true, fromEnv, profile),
		"a scripted run takes the inherited base URL over the profile's proxy")
	c.Eq(profile, resolveClaudeURL("", true, "", profile),
		"with no env base URL a scripted run still falls back to the profile")
	c.Eq(profile, resolveClaudeURL("", false, fromEnv, profile),
		"an interactive run keeps the profile's proxy: the env var is not its endpoint")
}

// TestScriptedClaudeResolvesNoProfile pins the isolation property a capture
// depends on: a scripted run must work on a host with no rafiki config at all,
// so it resolves no profile, mints no bootstrap one, and takes its endpoint and
// credential from flags or the environment only.
func TestScriptedClaudeResolvesNoProfile(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-from-env")

	orig := runClaudeTurnsFn
	t.Cleanup(func() { runClaudeTurnsFn = orig })
	var got claudeTurnsOptions
	runClaudeTurnsFn = func(o claudeTurnsOptions) error {
		got = o
		return nil
	}

	cmd := newClaudeCmd()
	c.Require().NoError(cmd.ParseFlags([]string{"--url", "http://127.0.0.1:9", "--turn", "hello"}),
		"parse flags")
	c.Require().NoError(runClaude(cmd, nil), "a scripted run must not need a profile")

	c.EqDeep([]string{"hello"}, got.Turns, "the prompt must reach the driver")
	argv := strings.Join(got.Args, " ")
	c.StrContains(argv, "--input-format stream-json", "a scripted run is the headless contract")
	c.StrContains(argv, "--output-format stream-json", "a scripted run is the headless contract")

	// The capture is kept as small and as reproducible as possible.
	for _, want := range []string{
		"--bare",                   // no CLAUDE.md, hooks, LSP, plugins, memory
		"--no-session-persistence", // nothing written outside the record dir
		"--strict-mcp-config",      // no inherited MCP servers
		"--exclude-dynamic-system-prompt-sections",
	} {
		c.StrContains(argv, want, "a scripted run must minimise what the request carries")
	}
	c.StrContains(argv, `--mcp-config {"mcpServers":{}}`, "every MCP server must be excluded")

	joined := strings.Join(got.Env, "\n")
	c.StrContains(joined, "ANTHROPIC_BASE_URL=http://127.0.0.1:9", "the endpoint must be the one given")
	c.StrContains(joined, "ANTHROPIC_API_KEY=sk-ant-from-env",
		"a --bare session authenticates with ANTHROPIC_API_KEY; the env credential becomes that key")
	c.NotStrContains(joined, "ANTHROPIC_AUTH_TOKEN", "bare mode never reads the bearer form")

	// The positive proof that nothing resolved a profile: resolution either
	// bootstraps profiles.toml or leaves it absent, and this run must leave it
	// absent. (A sibling test that already consumed profileOnce would resolve
	// from ITS temp dir, so this check is about this run's own config tree.)
	_, err := os.Stat(profile.ProfilesFile())
	c.True(os.IsNotExist(err), "a scripted run must not create a profile file (stat: %v)", err)
}

// TestScriptedClaudeWithoutAKeyKeepsOAuth is the OAuth-machine case: with no
// API key anywhere there is nothing to force, so the session runs on Claude
// Code's own login — and --bare, which only reads ANTHROPIC_API_KEY, must not be
// passed, or the run would fail on a host whose credential is perfectly good.
func TestScriptedClaudeWithoutAKeyKeepsOAuth(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	orig := runClaudeTurnsFn
	t.Cleanup(func() { runClaudeTurnsFn = orig })
	var got claudeTurnsOptions
	runClaudeTurnsFn = func(o claudeTurnsOptions) error {
		got = o
		return nil
	}

	cmd := newClaudeCmd()
	c.Require().NoError(cmd.ParseFlags([]string{"--url", "http://127.0.0.1:9", "--turn", "hi"}),
		"parse flags")
	c.Require().NoError(runClaude(cmd, nil), "a scripted run must not require an API key")

	argv := strings.Join(got.Args, " ")
	c.NotStrContains(argv, "--bare", "bare mode needs an API key, so it must not be forced on an OAuth host")
	c.StrContains(argv, "--no-session-persistence", "the other minimisation still applies")
	c.StrContains(argv, "--strict-mcp-config", "MCP servers are still excluded")

	joined := strings.Join(got.Env, "\n")
	c.NotStrContains(joined, "ANTHROPIC_API_KEY=", "nothing may be forced into the API key")
	c.NotStrContains(joined, "ANTHROPIC_AUTH_TOKEN=", "no bearer either: the session keeps its own login")
}
