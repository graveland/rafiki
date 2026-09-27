// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// TestMain isolates the client state directory for the whole package.
//
// buildSpawnRequest consults the remembered model, so without this the tests
// read the developer's real ~/.local/state/rafiki/client-state.json -- and a
// model remembered from ordinary use then leaks into a test's expected
// precedence. That is not hypothetical: it made the old model-precedence tests fail
// with a model no fixture mentions.
//
// Package-wide, because the coupling is inside the request builder rather than
// in any one test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rafiki-cli-state-")
	if err != nil {
		panic(err)
	}
	// XDG_CONFIG_HOME too, not just STATE: profile resolution reads
	// profiles.toml and BOOTSTRAP WRITES ONE. Without this, running the unit
	// tests creates a profiles.toml in the developer's real ~/.config/rafiki.
	os.Setenv("XDG_STATE_HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir)
	// Defense in depth, not a replacement for isolateProfiles(t): every test
	// that reaches resolveProfile still needs isolateProfiles for its own
	// isolated config/state tree. But a developer with e.g. RAFIKI_URL
	// exported in their shell (this repo's own .env, sourced by `make check`,
	// sets several of these) hits mustProfile's os.Exit(2) -- silently
	// truncating the WHOLE test binary -- in any test that happens to skip
	// calling isolateProfiles. Blanking these package-wide, once, means a
	// missing per-test call degrades safely instead.
	for _, v := range []string{
		paths.URL, paths.Token, paths.Socket,
		paths.DefaultModel, paths.DefaultPreset, paths.DefaultLabels,
		"RAFIKI_PROFILE",
	} {
		os.Setenv(v, "")
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// A bare `rafiki` (no subcommand) is `rafiki attach` with nothing to focus.
func TestRootRunEIsAttach(t *testing.T) {
	c := assert.NewAborting(t)
	root := newRootCmd()
	got := reflect.ValueOf(root.RunE).Pointer()
	want := reflect.ValueOf(runAttach).Pointer()
	c.Eq(want, got, "root's RunE is not runAttach; a bare `rafiki` would no longer behave like `rafiki attach`")
	c.NoError(root.Args(root, nil), "bare `rafiki` must be accepted")
}

// Args has to be set for RunE to ever see zero args (cobra defaults to
// ArbitraryArgs there), which means root's own custom Args func is now the
// only thing standing between a mistyped subcommand and silently trying to
// attach to a child literally named after the typo. It must still read as a
// typo, exactly as it did when cobra's own unmatched-subcommand check ran.
func TestRootArgsRejectsUnknownCommand(t *testing.T) {
	c := assert.NewAborting(t)
	root := newRootCmd()
	err := root.Args(root, []string{"bogus"})
	c.Error(err, "want an error for an unrecognised subcommand")
	c.StrContains(err.Error(), `unknown command "bogus"`, "error")
}

// A near-miss should still get the "Did you mean" nudge cobra's own
// legacyArgs gives -- rootArgs reproduces it by hand, so it is the one thing
// most likely to bitrot silently if a future cobra upgrade changes the
// wording this depends on.
func TestRootArgsSuggestsCloseMatches(t *testing.T) {
	root := newRootCmd()
	err := root.Args(root, []string{"lsit"})
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "list"), "error = %v, want a suggestion naming `list`", err)
}
