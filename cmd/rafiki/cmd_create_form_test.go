// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func createCmdFor(t *testing.T, argv ...string) (*cobra.Command, []string) {
	t.Helper()
	cmd := newCreateCmd()
	cmd.SetArgs(argv)
	if err := cmd.ParseFlags(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return cmd, cmd.Flags().Args()
}

// Bare create is the one invocation with nothing to go on.
func TestBareCreateWantsTheForm(t *testing.T) {
	cmd, args := createCmdFor(t)
	if !wantsCreateForm(cmd, args, true) {
		t.Error("bare create should open the form")
	}
}

// Anything that SHAPES the child is a statement of intent, so create honours
// it rather than asking again.
func TestShapingFlagsSuppressTheForm(t *testing.T) {
	for _, argv := range [][]string{
		{"reviewer"},
		{"--model", "anthropic/claude-opus-5"},
		{"--kind", "claude"},
		{"--detached"},
		{"--cwd", "/tmp"},
	} {
		cmd, args := createCmdFor(t, argv...)
		if wantsCreateForm(cmd, args, true) {
			t.Errorf("%v should spawn directly, not open the form", argv)
		}
	}
}

// A flag that says what happens AFTER the spawn is not a shaping flag.
func TestNonShapingFlagsStillOpenTheForm(t *testing.T) {
	cmd, args := createCmdFor(t, "--keep-on-exit")
	if !wantsCreateForm(cmd, args, true) {
		t.Error("--keep-on-exit describes exit behaviour, not the child")
	}
}

// -i forces the form even with shaping flags, which is how you get a PREFILLED
// form rather than no form at all.
func TestInteractiveForcesTheFormEvenWithFlags(t *testing.T) {
	cmd, args := createCmdFor(t, "-i", "--kind", "claude")
	if !wantsCreateForm(cmd, args, true) {
		t.Error("-i should force the form")
	}
}

// A form cannot render into a pipe, so a non-TTY always spawns directly --
// otherwise create in a script would hang on a screen nobody sees.
func TestNonTTYNeverOpensTheForm(t *testing.T) {
	for _, argv := range [][]string{{}, {"-i"}} {
		cmd, args := createCmdFor(t, argv...)
		if wantsCreateForm(cmd, args, false) {
			t.Errorf("%v opened the form with no terminal", argv)
		}
	}
}

// Every name in shapingFlags must exist, or the guard silently stops guarding
// that flag: Lookup returns nil and the loop skips it.
func TestEveryShapingFlagExists(t *testing.T) {
	cmd := newCreateCmd()
	for _, name := range shapingFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("shapingFlags names %q, which create does not define", name)
		}
	}
}

// --executor decides what child gets spawned and where, so it suppresses the
// form like any other shaping flag (reachable with the form only via -i).
func TestExecutorFlagSuppressesTheForm(t *testing.T) {
	cmd, args := createCmdFor(t, "--executor", "greyshift")
	if wantsCreateForm(cmd, args, true) {
		t.Error("--executor should spawn directly, not open the form")
	}
}

// The session executor is a fundi-only offer. A launch-required kind can never
// be served by it (it never advertises LaunchKinds), so the form path must not
// stand one up there -- the exact gap the executor-selection plan's Task 6
// review called out.
func TestWantsSessionExecutor(t *testing.T) {
	for _, tc := range []struct {
		name            string
		selector, kind  string
		noLocalExecutor bool
		want            bool
		executorRef     string
	}{
		{"fundi with nothing else is the zero-config default", "", "fundi", false, true, ""},
		{"claude never gets the session executor", "", "claude", false, false, ""},
		{"an explicit selector opts out entirely", "owner=brent,env=prod", "fundi", false, false, ""},
		{"an explicit executor opts out entirely", "", "fundi", false, false, "greyshift"},
		{"--no-local-executor opts out entirely", "", "fundi", true, false, ""},
		{"an unknown kind gets nothing", "", "", false, false, ""},
	} {
		if got := wantsSessionExecutor(tc.selector, tc.executorRef, tc.kind, tc.noLocalExecutor); got != tc.want {
			t.Errorf("%s: wantsSessionExecutor(%q, %q, %v) = %v, want %v",
				tc.name, tc.selector, tc.kind, tc.noLocalExecutor, got, tc.want)
		}
	}
}
