package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindDaemonBinaryFromSibling verifies that a daemon binary sitting next to
// the specified "self" path is returned without falling through to PATH.
func TestFindDaemonBinaryFromSibling(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "fundid")
	if err := os.WriteFile(sibling, []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := findDaemonBinaryFrom(filepath.Join(dir, "fundi"))
	if err != nil {
		t.Fatalf("expected sibling lookup to succeed: %v", err)
	}
	if got != sibling {
		t.Errorf("got %s, want %s", got, sibling)
	}
}

// TestFindDaemonBinaryNeverPicksTheClient is the regression test for the one
// way this rename could break silently.
//
// The client is `fundi` and the daemon is `fundid`, and `make install` puts
// both in the same directory. A sibling lookup for the wrong name therefore
// always succeeds — it finds the client — and `service install` writes a unit
// pointing the service at the CLI. Nothing fails until launchd or systemd
// actually starts it, long after the command that caused it returned 0.
func TestFindDaemonBinaryNeverPicksTheClient(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "fundi")
	if err := os.WriteFile(client, []byte("fake client"), 0755); err != nil {
		t.Fatal(err)
	}

	// Only the client exists. The lookup must not settle for it — it either
	// finds a real fundid on PATH or fails.
	got, err := findDaemonBinaryFrom(client)
	if err == nil && got == client {
		t.Fatalf("findDaemonBinaryFrom returned the client binary %q as the daemon", got)
	}
	if err == nil && filepath.Base(got) != "fundid" {
		t.Errorf("resolved daemon %q is not named fundid", got)
	}
}

// TestFindDaemonBinaryPrefersDaemonWhenBothPresent covers the layout `make
// install` actually produces: both binaries side by side in one directory.
// That is the arrangement nearly every real user ends up with, so it is the
// one the lookup most has to get right.
func TestFindDaemonBinaryPrefersDaemonWhenBothPresent(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "fundi")
	daemon := filepath.Join(dir, "fundid")
	for _, p := range []string{client, daemon} {
		if err := os.WriteFile(p, []byte("fake"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := findDaemonBinaryFrom(client)
	if err != nil {
		t.Fatalf("expected sibling lookup to succeed: %v", err)
	}
	if got != daemon {
		t.Errorf("got %s, want %s (the daemon, not the client)", got, daemon)
	}
}

// TestFindDaemonBinaryNoSibling verifies that when no sibling exists, the
// function either succeeds via PATH or returns a clear "not found" error.
func TestFindDaemonBinaryNoSibling(t *testing.T) {
	dir := t.TempDir() // empty — no daemon binary here

	got, err := findDaemonBinaryFrom(filepath.Join(dir, "fundi"))
	if err != nil {
		// Expected when the daemon is not on PATH.
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("unexpected error message: %v", err)
		}
		return
	}
	// If it succeeded (the daemon is on PATH), the result must be absolute.
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %s", got)
	}
}

// TestFindDaemonBinaryEmptySelf confirms that an empty self path falls
// through cleanly to PATH lookup rather than panicking.
func TestFindDaemonBinaryEmptySelf(t *testing.T) {
	_, err := findDaemonBinaryFrom("")
	// Either finds via PATH (nil error) or returns a useful error. Either is fine.
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildPathEnvContainsStandardPaths verifies standard directories are present.
func TestBuildPathEnvContainsStandardPaths(t *testing.T) {
	result := buildPathEnv()

	for _, want := range []string{"/usr/bin", "/bin"} {
		if !strings.Contains(result, want) {
			t.Errorf("buildPathEnv() missing %s; got %s", want, result)
		}
	}

	parts := strings.Split(result, ":")
	if len(parts) < 3 {
		t.Errorf("buildPathEnv() too few entries (%d): %q", len(parts), result)
	}
}

// TestBuildPathEnvNoDuplicates verifies no directory appears twice.
func TestBuildPathEnvNoDuplicates(t *testing.T) {
	result := buildPathEnv()
	parts := strings.Split(result, ":")
	seen := make(map[string]bool)
	for _, p := range parts {
		if seen[p] {
			t.Errorf("buildPathEnv() has duplicate entry %q in %q", p, result)
		}
		seen[p] = true
	}
}

// TestNewServiceBackend verifies the factory returns a non-nil backend with
// a valid log path.
func TestNewServiceBackend(t *testing.T) {
	b := newServiceBackend()
	if b == nil {
		t.Fatal("newServiceBackend() returned nil")
	}

	lp := b.LogPath()
	if lp == "" {
		t.Error("LogPath() returned empty string")
	}
	if filepath.Base(lp) != "controller.log" {
		t.Errorf("LogPath() base = %q, want controller.log", filepath.Base(lp))
	}
	if !filepath.IsAbs(lp) {
		t.Errorf("LogPath() should be absolute, got %s", lp)
	}
}
