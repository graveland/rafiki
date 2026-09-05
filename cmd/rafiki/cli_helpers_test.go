package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

// TestDecideKillOnExit_Flags covers the non-interactive paths: flag overrides
// and non-TTY stdin. The interactive prompt path requires a real terminal and
// is exercised manually.
func TestDecideKillOnExit_KillFlag(t *testing.T) {
	// --kill-on-exit → true, regardless of other state.
	got, err := decideKillOnExit(true, false, "my-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("killOnExit=true: expected true, got false")
	}
}

func TestDecideKillOnExit_KeepFlag(t *testing.T) {
	// --keep-on-exit → false.
	got, err := decideKillOnExit(false, true, "my-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("keepOnExit=true: expected false, got true")
	}
}

func TestDecideKillOnExit_NonTTY_DefaultsToKeep(t *testing.T) {
	// When neither flag is set and stdin is not a TTY (as in a test runner),
	// decideKillOnExit should default to keep without prompting.
	if isStdinTTY() {
		t.Skip("stdin is a TTY; skipping non-TTY default test")
	}
	got, err := decideKillOnExit(false, false, "my-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("non-TTY stdin: expected false (keep), got true (kill)")
	}
}

func TestDecideKillOnExit_KillFlagBeatsKeep(t *testing.T) {
	// Cobra enforces mutual exclusivity, but the function itself is pure —
	// verify that killOnExit wins the short-circuit when both are accidentally true.
	got, err := decideKillOnExit(true, true, "my-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("killOnExit=true,keepOnExit=true: expected true (kill wins), got false")
	}
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
			gotKill, gotWarn := parseKillAnswer(input)
			if gotKill != tc.wantKill {
				t.Errorf("parseKillAnswer(%q) kill=%v, want %v", input, gotKill, tc.wantKill)
			}
			if gotWarn != tc.wantWarn {
				t.Errorf("parseKillAnswer(%q) warned=%v, want %v", input, gotWarn, tc.wantWarn)
			}
		})
	}
}

// TestDialDaemon_UsesTheProfilesSocket drives dialDaemon through a seeded
// local-socket profile: it must attempt the UDS dial named by the profile (and
// fail with a UDS-shaped error against a socket that doesn't exist), never
// treat the absence of a URL as anything but "dial the socket". This
// supersedes the old RAFIKI_URL-scheme gate (remoteDialURL, deleted): a
// profile's endpoint is either a socket or an https:// URL by construction
// (pkg/profile's validate), so there is no scheme ambiguity left to pin.
func TestDialDaemon_UsesTheProfilesSocket(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	sock := filepath.Join(t.TempDir(), "no-such.sock")
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	_, err := dialDaemon(context.Background(), nil)
	if err == nil {
		t.Fatal("expected a dial error against a nonexistent socket")
	}
	if !strings.Contains(err.Error(), "no-such.sock") {
		t.Errorf("expected the UDS dial failure to name the socket path, got: %v", err)
	}
}

// B4 deleted the TypeScript TUI that rafiki-attach built. The helpers that
// shelled out to it must go with it, or a non-detached `rafiki create` fails
// at runtime telling the user to run a make target that no longer exists.
func TestRafikiAttachSubprocessHelpersAreGone(t *testing.T) {
	src, err := os.ReadFile("cli_helpers.go")
	if err != nil {
		t.Fatalf("read cli_helpers.go: %v", err)
	}
	for _, gone := range []string{"findRafikiAttach", "execRafikiAttach", "attachEnv"} {
		if strings.Contains(string(src), gone) {
			t.Errorf("%s still exists; it shells out to the deleted rafiki-attach binary", gone)
		}
	}
}
