// SPDX-License-Identifier: Apache-2.0

package claudeargv

import (
	"slices"
	"strings"
	"testing"
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
		if !slices.Contains(got, want) {
			t.Errorf("Build() = %v, missing %q", got, want)
		}
	}
}

func TestBuildOmitsEmptyOptionalFlags(t *testing.T) {
	got := strings.Join(Build(Params{}), " ")
	for _, unwanted := range []string{"--model", "--resume", "--permission-mode"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("Build(zero) = %q, should not carry %q", got, unwanted)
		}
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
	got := Build(Params{PermissionMode: "bypassPermissions"})
	if !slices.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("Build(bypassPermissions) = %v, want --dangerously-skip-permissions", got)
	}
	if slices.Contains(got, "--permission-mode") {
		t.Errorf("Build(bypassPermissions) = %v, should not also pass --permission-mode", got)
	}
}

func TestBuildPassesOtherPermissionModesThrough(t *testing.T) {
	assertPair(t, Build(Params{PermissionMode: "acceptEdits"}), "--permission-mode", "acceptEdits")
}

// Build must not hand its caller a slice that aliases package state; a caller
// appending to the result would corrupt the next build.
func TestBuildReturnsAFreshSlice(t *testing.T) {
	a := Build(Params{Model: "m1"})
	b := Build(Params{Model: "m2"})
	if slices.Equal(a, b) {
		t.Fatal("two builds with different models returned equal argv")
	}
	_ = append(a, "--sentinel")
	if slices.Contains(Build(Params{Model: "m1"}), "--sentinel") {
		t.Error("appending to a returned slice leaked into a later build")
	}
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
	if !slices.Contains(Build(Params{}), "--dangerously-skip-permissions") {
		t.Fatalf("Build(zero) = %v, want --dangerously-skip-permissions by default", Build(Params{}))
	}
}

func TestBuildAppendsExtraArgsLast(t *testing.T) {
	argv := Build(Params{Model: "claude-sonnet-5", ExtraArgs: []string{"--foo", "bar"}})
	if len(argv) < 2 || argv[len(argv)-2] != "--foo" || argv[len(argv)-1] != "bar" {
		t.Fatalf("want ExtraArgs last, got %v", argv)
	}
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
	if !slices.Equal(got, want) {
		t.Errorf("Build(Params{Model: %q}) = %v, want %v", "x", got, want)
	}
}

// ModelArgs REPLACES the plain --model pair, never adds a second one — a
// duplicated --model would depend on claude's last-flag-wins tie-break instead
// of being unambiguous.
func TestBuildModelArgsSuppressesPlainModel(t *testing.T) {
	got := Build(Params{Model: "x", ModelArgs: []string{"--model", "y"}})
	n := 0
	for i, a := range got {
		if a != "--model" {
			continue
		}
		n++
		if i+1 >= len(got) || got[i+1] != "y" {
			t.Errorf("argv %v: --model is not followed by %q", got, "y")
		}
	}
	if n != 1 {
		t.Errorf("Build(Model: x, ModelArgs: [--model y]) = %v, want exactly one --model, got %d", got, n)
	}
}

// --mcp-config is variadic, so the value must ride the SAME argv element via
// '=' — a two-element pair would swallow whatever flag follows it.
func TestBuildMCPConfigIsSingleElement(t *testing.T) {
	got := Build(Params{MCPConfig: `{"a":1}`})
	n := 0
	for _, a := range got {
		if !strings.HasPrefix(a, "--mcp-config") {
			continue
		}
		n++
		if a != `--mcp-config={"a":1}` {
			t.Errorf("Build(MCPConfig) = %v, want one element --mcp-config={\"a\":1}, got %q", got, a)
		}
	}
	if n != 1 {
		t.Errorf("Build(MCPConfig) = %v, want exactly one --mcp-config element, got %d", got, n)
	}
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
		if slices.Contains(got, unwanted) {
			t.Errorf("Build(interactive) = %v, should not carry %q", got, unwanted)
		}
	}
}

func assertPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i, a := range argv {
		if a == flag {
			if i+1 >= len(argv) || argv[i+1] != value {
				t.Errorf("argv %v: %s is not followed by %q", argv, flag, value)
			}
			return
		}
	}
	t.Errorf("argv %v: missing %s", argv, flag)
}
