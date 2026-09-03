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
