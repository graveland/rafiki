package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestFindDaemonBinaryFromSibling verifies that a daemon binary sitting next to
// the specified "self" path is returned without falling through to PATH.
func TestFindDaemonBinaryFromSibling(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "rafikid")
	if err := os.WriteFile(sibling, []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := findDaemonBinaryFrom(filepath.Join(dir, "rafiki"))
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
// The client is `rafiki` and the daemon is `rafikid`, and `make install` puts
// both in the same directory. A sibling lookup for the wrong name therefore
// always succeeds — it finds the client — and `service install` writes a unit
// pointing the service at the CLI. Nothing fails until launchd or systemd
// actually starts it, long after the command that caused it returned 0.
func TestFindDaemonBinaryNeverPicksTheClient(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "rafiki")
	if err := os.WriteFile(client, []byte("fake client"), 0755); err != nil {
		t.Fatal(err)
	}

	// Only the client exists. The lookup must not settle for it — it either
	// finds a real rafikid on PATH or fails.
	got, err := findDaemonBinaryFrom(client)
	if err == nil && got == client {
		t.Fatalf("findDaemonBinaryFrom returned the client binary %q as the daemon", got)
	}
	if err == nil && filepath.Base(got) != "rafikid" {
		t.Errorf("resolved daemon %q is not named rafikid", got)
	}
}

// TestFindDaemonBinaryPrefersDaemonWhenBothPresent covers the layout `make
// install` actually produces: both binaries side by side in one directory.
// That is the arrangement nearly every real user ends up with, so it is the
// one the lookup most has to get right.
func TestFindDaemonBinaryPrefersDaemonWhenBothPresent(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "rafiki")
	daemon := filepath.Join(dir, "rafikid")
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

	got, err := findDaemonBinaryFrom(filepath.Join(dir, "rafiki"))
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

// ─── daemon environment capture ────────────────────────────────────────────────

func TestCaptureDaemonEnv_PicksDaemonScopedVars(t *testing.T) {
	got, _ := captureDaemonEnv([]string{
		"RAFIKI_DB=postgres://u@localhost/rafiki?sslmode=disable",
		"RAFIKI_DEFAULT_MODEL=anthropic/opus-latest",
		"PATH=/usr/bin",
		"UNRELATED=x",
	})
	want := map[string]string{
		"RAFIKI_DB":            "postgres://u@localhost/rafiki?sslmode=disable",
		"RAFIKI_DEFAULT_MODEL": "anthropic/opus-latest",
	}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// RAFIKI_CHILD_ID must never be baked into the unit: the daemon sets it per
// child and `rafikid agent` uses it as the default --ref, so a service-wide
// value would collide every child onto one conversation.
func TestCaptureDaemonEnv_ExcludesChildID(t *testing.T) {
	got, _ := captureDaemonEnv([]string{"RAFIKI_CHILD_ID=c_123", "RAFIKI_DB=x"})
	if _, ok := got["RAFIKI_CHILD_ID"]; ok {
		t.Error("RAFIKI_CHILD_ID was captured into the service environment")
	}
}

// Credentials do not belong in a world-readable unit file.
func TestCaptureDaemonEnv_ExcludesAPIKeys(t *testing.T) {
	got, _ := captureDaemonEnv([]string{"ANTHROPIC_API_KEY=sk-ant", "OPENROUTER_API_KEY=sk-or"})
	if len(got) != 0 {
		t.Errorf("captured credentials into the unit: %v", got)
	}
}

// An empty value is not the same as an absent one: writing RAFIKI_DB=""
// would turn a missing export into a configured-but-broken DSN.
func TestCaptureDaemonEnv_SkipsEmpty(t *testing.T) {
	got, _ := captureDaemonEnv([]string{"RAFIKI_DB="})
	if _, ok := got["RAFIKI_DB"]; ok {
		t.Error("an empty value was captured")
	}
}

func TestSortedEnv_IsDeterministic(t *testing.T) {
	m := map[string]string{"RAFIKI_SOCKET": "/s", "RAFIKI_DB": "db", "RAFIKI_PI_BINARY": "/pi"}
	first := sortedEnv(m)
	for range 20 { // map iteration order varies per range; the output must not
		if !slices.Equal(sortedEnv(m), first) {
			t.Fatal("sortedEnv is not deterministic across calls")
		}
	}
	if first[0].Key != "RAFIKI_DB" {
		t.Errorf("not sorted by key: %v", first)
	}
}

// No unit file can carry a newline: systemd's Environment= is line-based, and
// carrying it only on launchd would give a service that installs on one
// platform and not the other. Such values must be skipped and reported, not
// written into a unit that then fails to parse.
func TestCaptureDaemonEnv_SkipsNewlineValues(t *testing.T) {
	captured, skipped := captureDaemonEnv([]string{
		"RAFIKI_DEFAULT_LABELS=a=1\nb=2",
		"RAFIKI_DB=postgres://u@h/db",
	})
	if _, ok := captured["RAFIKI_DEFAULT_LABELS"]; ok {
		t.Error("a newline-bearing value was written into the unit")
	}
	if !slices.Contains(skipped, "RAFIKI_DEFAULT_LABELS") {
		t.Errorf("skipped = %v, want it to name RAFIKI_DEFAULT_LABELS", skipped)
	}
	// Skipping one must not cost the others.
	if captured["RAFIKI_DB"] != "postgres://u@h/db" {
		t.Error("an unrelated variable was lost alongside the skipped one")
	}
}
