package main

import (
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestDecideKillOnExit_Flags covers the non-interactive paths: flag overrides
// and non-TTY stdin. The interactive prompt path requires a real terminal and
// is exercised manually.
func TestDecideKillOnExit_KillFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	// --kill-on-exit → true, regardless of other state.
	got, err := decideKillOnExit(true, false, "my-session")
	c.Require().NoError(err, "unexpected error")
	c.True(got, "killOnExit=true: expected true, got false")
}

func TestDecideKillOnExit_KeepFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	// --keep-on-exit → false.
	got, err := decideKillOnExit(false, true, "my-session")
	c.Require().NoError(err, "unexpected error")
	c.False(got, "keepOnExit=true: expected false, got true")
}

func TestDecideKillOnExit_NonTTY_DefaultsToKeep(t *testing.T) {
	c := assert.NewCollecting(t)
	// When neither flag is set and stdin is not a TTY (as in a test runner),
	// decideKillOnExit should default to keep without prompting.
	if isStdinTTY() {
		t.Skip("stdin is a TTY; skipping non-TTY default test")
	}
	got, err := decideKillOnExit(false, false, "my-session")
	c.Require().NoError(err, "unexpected error")
	c.False(got, "non-TTY stdin: expected false (keep), got true (kill)")
}

func TestDecideKillOnExit_KillFlagBeatsKeep(t *testing.T) {
	c := assert.NewCollecting(t)
	// Cobra enforces mutual exclusivity, but the function itself is pure —
	// verify that killOnExit wins the short-circuit when both are accidentally true.
	got, err := decideKillOnExit(true, true, "my-session")
	c.Require().NoError(err, "unexpected error")
	c.True(got, "killOnExit=true,keepOnExit=true: expected true (kill wins), got false")
}

// TestParseKillAnswer exercises the y/N prompt answer parser.
// decideKillOnExit applies strings.TrimSpace(strings.ToLower(...)) before
// calling parseKillAnswer, so this function only ever sees lowercase input.
// "Y" → "y" and "YES" → "yes" at the call site, both produce kill=true.
func TestParseKillAnswer(t *testing.T) {
	tests := []struct {
		input    string
		wantKill bool
		wantWarn bool
	}{
		// Explicit yes (normalised by caller) → terminate
		{"y", true, false},
		{"yes", true, false},

		// Explicit no → keep
		{"n", false, false},
		{"no", false, false},

		// Empty (Enter) → keep
		{"", false, false},

		// Anything unrecognised → keep with warning
		{"maybe", false, true},
		{"terminate", false, true},
		{"k", false, true},
		{"keep", false, true},
		{"t", false, true},
	}
	for _, tc := range tests {
		input := tc.input
		t.Run(input, func(t *testing.T) {
			c := assert.NewCollecting(t)
			gotKill, gotWarn := parseKillAnswer(input)
			c.Eq(tc.wantKill, gotKill, "parseKillAnswer(%q) kill=%v, want", input, gotKill)
			c.Eq(tc.wantWarn, gotWarn, "parseKillAnswer(%q) warned=%v, want", input, gotWarn)
		})
	}
}

// TestDialDaemon_UsesTheProfilesSocket was deleted with dialDaemon itself
// (W4a): 4.5's Connect cutover removed completion's call, leaving dialDaemon
// with no non-test caller, and its doc comment described a caller that no
// longer existed.

// B4 deleted the TypeScript TUI that rafiki-attach built. The helpers that
// shelled out to it must go with it, or a non-detached `rafiki create` fails
// at runtime telling the user to run a make target that no longer exists.
func TestRafikiAttachSubprocessHelpersAreGone(t *testing.T) {
	c := assert.NewCollecting(t)
	src, err := os.ReadFile("cli_helpers.go")
	c.Require().NoError(err, "read cli_helpers.go")
	for _, gone := range []string{"findRafikiAttach", "execRafikiAttach", "attachEnv"} {
		c.NotStrContains(string(src), gone, "%s still exists; it shells out to the deleted rafiki-attach binary", gone)
	}
}
