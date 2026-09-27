// SPDX-License-Identifier: Apache-2.0

package claudeargv

import (
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

// The base flags are what makes claude speak the stream-json protocol daraja
// relays and rafikid parses. A build that omits any of them produces a child
// that runs and is unintelligible, which is far worse than one that fails.
func TestBuildAlwaysCarriesTheStreamJSONContract(t *testing.T) {
	got := Build(Params{})
	for _, want := range []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	} {
		assert.NewCollecting(t).Contains(got, want, "Build()")
	}
}

func TestBuildOmitsEmptyOptionalFlags(t *testing.T) {
	got := strings.Join(Build(Params{}), " ")
	for _, unwanted := range []string{"--model", "--resume", "--permission-mode"} {
		assert.NewCollecting(t).NotStrContains(got, unwanted, "Build(zero)")
	}
}

func TestBuildCarriesModelAndResume(t *testing.T) {
	got := Build(Params{Model: "claude-opus-5", ResumeSession: "abc-123"})
	assertPair(t, got, "--model", "claude-opus-5")
	assertPair(t, got, "--resume", "abc-123")
}

// bypassPermissions is the one permission mode that is a bare flag rather than
// a --permission-mode value; claude rejects `--permission-mode
// bypassPermissions`.
func TestBuildMapsBypassToItsOwnFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	got := Build(Params{PermissionMode: "bypassPermissions"})
	c.Contains(got, "--dangerously-skip-permissions", "Build(bypassPermissions)")
	c.NotContains(got, "--permission-mode", "Build(bypassPermissions)")
}

func TestBuildPassesOtherPermissionModesThrough(t *testing.T) {
	assertPair(t, Build(Params{PermissionMode: "acceptEdits"}), "--permission-mode", "acceptEdits")
}

// Build must not hand its caller a slice that aliases package state; a caller
// appending to the result would corrupt the next build.
func TestBuildReturnsAFreshSlice(t *testing.T) {
	c := assert.NewCollecting(t)
	a := Build(Params{Model: "m1"})
	b := Build(Params{Model: "m2"})
	c.Require().False(slices.Equal(a, b), "two builds with different models returned equal argv")
	_ = append(a, "--sentinel")
	c.NotContains(Build(Params{Model: "m1"}), "--sentinel", "appending to a returned slice leaked into a later build")
}

// AskUserQuestion has no headless renderer and burns a turn if left callable
// — this must be true regardless of what else the caller asks to disallow.
func TestBuildAlwaysDisallowsAskUserQuestion(t *testing.T) {
	assertPair(t, Build(Params{}), "--disallowedTools", "AskUserQuestion")
	assertPair(t, Build(Params{DisallowedTools: []string{"Bash"}}), "--disallowedTools", "AskUserQuestion,Bash")
}

// A daemon-managed child has no human to answer an interactive permission
// prompt, so the empty (unset) PermissionMode must default to bypass, not to
// "ask and hang forever".
func TestBuildDefaultsToBypassPermissions(t *testing.T) {
	assert.NewAborting(t).Contains(Build(Params{}), "--dangerously-skip-permissions", "Build(zero)")
}

func TestBuildAppendsExtraArgsLast(t *testing.T) {
	argv := Build(Params{Model: "claude-sonnet-5", ExtraArgs: []string{"--foo", "bar"}})
	assert.NewAborting(t).False(len(argv) < 2 || argv[len(argv)-2] != "--foo" || argv[len(argv)-1] != "bar", "want ExtraArgs last, got %v", argv)
}

// The additive wave's pin: a Params that sets only Model must produce argv
// byte-identical to the pre-Mode, pre-MCPConfig builder, so every existing
// caller that leaves the new fields zero is unaffected.
func TestBuildHeadlessUnchanged(t *testing.T) {
	got := Build(Params{Model: "x"})
	want := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "x",
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
	}
	assert.NewCollecting(t).EqDiff(want, got, "Build(Params{Model: %q}) = %v, want", "x", got)
}

// ModelArgs REPLACES the plain --model pair, never adds a second one — a
// duplicated --model would depend on claude's last-flag-wins tie-break instead
// of being unambiguous.
func TestBuildModelArgsSuppressesPlainModel(t *testing.T) {
	c := assert.NewCollecting(t)
	got := Build(Params{Model: "x", ModelArgs: []string{"--model", "y"}})
	n := 0
	for i, a := range got {
		if a != "--model" {
			continue
		}
		n++
		c.False(i+1 >= len(got) || got[i+1] != "y", "argv %v: --model is not followed by %q", got, "y")
	}
	c.Eq(1, n, "Build(Model: x, ModelArgs: [--model y]) = %v, want exactly one --model, got", got)
}

// --mcp-config is variadic, so the value must ride the SAME argv element via
// '=' — a two-element pair would swallow whatever flag follows it.
func TestBuildMCPConfigIsSingleElement(t *testing.T) {
	c := assert.NewCollecting(t)
	got := Build(Params{MCPConfig: `{"a":1}`})
	n := 0
	for _, a := range got {
		if !strings.HasPrefix(a, "--mcp-config") {
			continue
		}
		n++
		c.Eq(`--mcp-config={"a":1}`, a, "Build(MCPConfig) = %v, want one element --mcp-config={\"a\":1}, got", got)
	}
	c.Eq(1, n, "Build(MCPConfig) = %v, want exactly one --mcp-config element, got", got)
}

// The coordination prompt rides a proxied child's argv, keyed on the MCP
// config the proxy decided: it names the agent-control tools, so a child
// without the MCP surface must not carry it. This pins the local-subprocess
// half of the gate; the daraja half is pinned by test/integration's
// TestClaudeArgvIdenticalAcrossPaths, which drives both mappings.
func TestParamsFromSpawnRequestInjectsTheCoordinationPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	_, proxied := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	c.Require().NotEq("", proxied.MCPConfig, "fixture: ClaudeEnv produced no MCP config for a proxied session")

	got := ParamsFromSpawnRequest(protocol.SpawnRequest{}, proxied)
	c.Eq(CoordinationPrompt, got.AppendSystemPrompt, "proxied, no caller prompt: AppendSystemPrompt")

	got = ParamsFromSpawnRequest(protocol.SpawnRequest{AppendSystemPrompt: "be terse"}, proxied)
	want := CoordinationPrompt + "\n\nbe terse"
	c.Eq(want, got.AppendSystemPrompt, "proxied, caller prompt: AppendSystemPrompt")

	got = ParamsFromSpawnRequest(protocol.SpawnRequest{AppendSystemPrompt: "be terse"}, proxyenv.Values{})
	c.Eq("be terse", got.AppendSystemPrompt, "unproxied: AppendSystemPrompt")
	got = ParamsFromSpawnRequest(protocol.SpawnRequest{}, proxyenv.Values{})
	c.Eq("", got.AppendSystemPrompt, "unproxied, no caller prompt: AppendSystemPrompt")
}

// TestCoordinationPromptMentionsPresets pins the preset sentence: the prompt
// names the preset_* tools in its tool list and points the child at spawning
// by preset rather than choosing models — the same preference the MCP face's
// agent_spawn description carries (its blueprint text ends with the preset
// paragraph), per CoordinationPrompt's doc comment.
func TestCoordinationPromptMentionsPresets(t *testing.T) {
	for _, want := range []string{"preset_list", "agent_spawn's preset", "preset_*"} {
		assert.NewCollecting(t).StrContains(CoordinationPrompt, want, "CoordinationPrompt")
	}
}

// The flag is last-wins, so the merge must land as ONE --append-system-prompt
// element carrying both texts — a second element would silently drop whichever
// text came first.
func TestBuildRendersOneAppendSystemPromptElement(t *testing.T) {
	c := assert.NewCollecting(t)
	_, vals := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	argv := Build(ParamsFromSpawnRequest(protocol.SpawnRequest{AppendSystemPrompt: "be terse"}, vals))
	n := 0
	for i, a := range argv {
		if a != "--append-system-prompt" {
			continue
		}
		n++
		c.Require().Less(len(argv), i+1, "argv %v: --append-system-prompt has no value", argv)
		v := argv[i+1]
		for _, want := range []string{CoordinationPrompt, "be terse"} {
			c.StrContains(v, want, "--append-system-prompt value")
		}
	}
	c.Eq(1, n, "argv %v: want exactly one --append-system-prompt element, got", argv)
}

// ModeInteractive keeps the TTY: a human answers permission prompts, so none
// of the headless stream-json contract, the bypass default or the
// disallowedTools pair may appear.
func TestBuildInteractiveOmitsHeadlessFlags(t *testing.T) {
	got := Build(Params{Mode: ModeInteractive})
	for _, unwanted := range []string{
		"-p",
		"--input-format",
		"--output-format",
		"--verbose",
		"--dangerously-skip-permissions",
		"--disallowedTools",
	} {
		assert.NewCollecting(t).NotContains(got, unwanted, "Build(interactive)")
	}
}

func assertPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i, a := range argv {
		if a == flag {
			assert.NewCollecting(t).False(i+1 >= len(argv) || argv[i+1] != value, "argv %v: %s is not followed by %q", argv, flag, value)
			return
		}
	}
	t.Errorf("argv %v: missing %s", argv, flag)
}
