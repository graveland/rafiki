package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
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
	got, _, _ := captureDaemonEnv([]string{
		"RAFIKI_SOCKET=/tmp/rafiki.sock",
		"RAFIKI_DEFAULT_MODEL=anthropic/opus-latest",
		"PATH=/usr/bin",
		"UNRELATED=x",
	})
	want := map[string]string{
		"RAFIKI_SOCKET":        "/tmp/rafiki.sock",
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
	got, _, _ := captureDaemonEnv([]string{"RAFIKI_CHILD_ID=c_123", "RAFIKI_DB=x"})
	if _, ok := got["RAFIKI_CHILD_ID"]; ok {
		t.Error("RAFIKI_CHILD_ID was captured into the service environment")
	}
}

// The regression this whole change exists for. A DSN carries a password and
// unit files are 0644, so RAFIKI_DB must be routed to service.env — which the
// list it used to sit in already said, about the API keys, while capturing it.
func TestCaptureDaemonEnv_RoutesTheDSNToSecrets(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{
		"RAFIKI_DB=postgres://u:hunter2@localhost/rafiki",
		"RAFIKI_DEFAULT_MODEL=anthropic/opus-latest",
	})
	if _, ok := unit["RAFIKI_DB"]; ok {
		t.Error("RAFIKI_DB was captured into the world-readable unit")
	}
	if secret["RAFIKI_DB"] != "postgres://u:hunter2@localhost/rafiki" {
		t.Errorf("secret[RAFIKI_DB] = %q, want the DSN", secret["RAFIKI_DB"])
	}
	if unit["RAFIKI_DEFAULT_MODEL"] != "anthropic/opus-latest" {
		t.Error("a non-secret variable stopped reaching the unit")
	}
}

// Credentials and tokens all take the same route.
func TestCaptureDaemonEnv_RoutesCredentialsToSecrets(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{
		"ANTHROPIC_API_KEY=sk-ant",
		"OPENROUTER_API_KEY=sk-or",
		"RAFIKI_TOKEN=client",
		"RAFIKI_SERVE_TOKEN=server",
	})
	if len(unit) != 0 {
		t.Errorf("credentials reached the unit: %v", unit)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "RAFIKI_TOKEN", "RAFIKI_SERVE_TOKEN"} {
		if _, ok := secret[k]; !ok {
			t.Errorf("%s was not routed to service.env", k)
		}
	}
}

// The newline restriction is a unit-file limitation, not a general one:
// service.env quotes and escapes, so a secret carrying a newline is written
// rather than skipped.
func TestCaptureDaemonEnv_SecretsAreNotNewlineRestricted(t *testing.T) {
	_, secret, skipped := captureDaemonEnv([]string{"RAFIKI_DB=postgres://u@h/db\n"})
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none: service.env can hold a newline", skipped)
	}
	if secret["RAFIKI_DB"] == "" {
		t.Error("a secret with a newline was dropped")
	}
}

// RAFIKI_PROXY_LISTEN is an address, not a secret, so it belongs in the unit —
// and until now install could not carry it across a reinstall at all.
func TestCaptureDaemonEnv_CapturesProxyListenIntoTheUnit(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{"RAFIKI_PROXY_LISTEN=127.0.0.1:8035"})
	if unit["RAFIKI_PROXY_LISTEN"] != "127.0.0.1:8035" {
		t.Errorf("unit[RAFIKI_PROXY_LISTEN] = %q, want the address", unit["RAFIKI_PROXY_LISTEN"])
	}
	if _, ok := secret["RAFIKI_PROXY_LISTEN"]; ok {
		t.Error("an address was treated as a credential")
	}
}

// An empty value is not the same as an absent one: writing RAFIKI_DB=""
// would turn a missing export into a configured-but-broken DSN. The rule
// applies to both destinations, so both a secret (RAFIKI_DB) and a unit var
// (RAFIKI_SOCKET) are exercised here — a regression that let an empty value
// through to just one of the two maps would otherwise go undetected.
func TestCaptureDaemonEnv_SkipsEmpty(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{"RAFIKI_DB=", "RAFIKI_SOCKET="})
	if _, ok := secret["RAFIKI_DB"]; ok {
		t.Error("an empty secret value was captured")
	}
	if _, ok := unit["RAFIKI_SOCKET"]; ok {
		t.Error("an empty unit value was captured")
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
	captured, _, skipped := captureDaemonEnv([]string{
		"RAFIKI_DEFAULT_LABELS=a=1\nb=2",
		"RAFIKI_SOCKET=/tmp/rafiki.sock",
	})
	if _, ok := captured["RAFIKI_DEFAULT_LABELS"]; ok {
		t.Error("a newline-bearing value was written into the unit")
	}
	if !slices.Contains(skipped, "RAFIKI_DEFAULT_LABELS") {
		t.Errorf("skipped = %v, want it to name RAFIKI_DEFAULT_LABELS", skipped)
	}
	// Skipping one must not cost the others.
	if captured["RAFIKI_SOCKET"] != "/tmp/rafiki.sock" {
		t.Error("an unrelated variable was lost alongside the skipped one")
	}
}

// `logs` prints and exits; `tail` is the one that follows. The default on
// --follow is the whole distinction, so it is worth pinning.
func TestServiceLogsPrintsAndExitsByDefault(t *testing.T) {
	f := newServiceLogsCmd().Flags().Lookup("follow")
	if f == nil {
		t.Fatal("--follow not registered on service logs")
	}
	if f.DefValue != "false" {
		t.Errorf("--follow default = %s, want false (logs prints and exits; use tail to follow)", f.DefValue)
	}
	if f.Shorthand != "f" {
		t.Errorf("--follow shorthand = %q, want \"f\"", f.Shorthand)
	}
}

func TestServiceTailIsRegistered(t *testing.T) {
	var found bool
	for _, c := range newServiceCmd().Commands() {
		if c.Name() == "tail" {
			found = true
			if c.Flags().Lookup("follow") != nil {
				t.Error("service tail should always follow, not carry a --follow flag")
			}
		}
	}
	if !found {
		t.Error("service tail not registered")
	}
}

func TestInstallReport_ListsEachDestination(t *testing.T) {
	spec := serviceSpec{
		ExtraEnv:  map[string]string{"RAFIKI_PROXY_KINDS": "pi,claude"},
		SecretEnv: map[string]string{"RAFIKI_DB": "postgres://u@h/db", "ANTHROPIC_API_KEY": "sk-ant"},
	}
	res := paths.MergeResult{
		Added:    []string{"ANTHROPIC_API_KEY", "RAFIKI_DB"},
		Existing: []string{"RAFIKI_SERVE_TOKEN"},
		Defined:  []string{"RAFIKI_SERVE_TOKEN"},
	}

	out := installReport(spec, "/cfg/service.env", res, nil)

	for _, want := range []string{"RAFIKI_PROXY_KINDS", "ANTHROPIC_API_KEY", "RAFIKI_DB", "RAFIKI_SERVE_TOKEN", "/cfg/service.env"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %s:\n%s", want, out)
		}
	}
	// The secret's VALUE must never be printed to a terminal or a scrollback.
	for _, secret := range []string{"postgres://u@h/db", "sk-ant"} {
		if strings.Contains(out, secret) {
			t.Errorf("report leaked a secret value %q:\n%s", secret, out)
		}
	}
}

// "Left alone" without this note quietly discards what the operator just
// exported, which is the confusing outcome this whole command exists to avoid.
func TestInstallReport_FlagsAConflictingValue(t *testing.T) {
	spec := serviceSpec{SecretEnv: map[string]string{"RAFIKI_DB": "postgres://new@h/db"}}
	res := paths.MergeResult{Conflict: []string{"RAFIKI_DB"}, Defined: []string{"RAFIKI_DB"}}

	out := installReport(spec, "/cfg/service.env", res, nil)

	if !strings.Contains(out, "RAFIKI_DB") {
		t.Fatalf("report does not mention the conflicting key:\n%s", out)
	}
	if !strings.Contains(out, "differs") {
		t.Errorf("report does not say the file's value differs from this shell's:\n%s", out)
	}
}

// The missing-DSN warning is about the actual failure condition: no DSN
// anywhere. A DSN already in service.env is not a problem to warn about.
func TestInstallReport_NoDSNWarningWhenTheFileAlreadyHasOne(t *testing.T) {
	spec := serviceSpec{SecretEnv: map[string]string{}} // nothing in this shell
	res := paths.MergeResult{Defined: []string{"RAFIKI_DB"}}

	if out := installReport(spec, "/cfg/service.env", res, nil); strings.Contains(out, "in-memory") {
		t.Errorf("warned about a missing DSN that service.env already defines:\n%s", out)
	}
}

func TestInstallReport_WarnsWhenNoDSNAnywhere(t *testing.T) {
	out := installReport(serviceSpec{SecretEnv: map[string]string{}}, "/cfg/service.env", paths.MergeResult{}, nil)
	if !strings.Contains(out, "in-memory") {
		t.Errorf("no missing-DSN warning when neither the shell nor the file has one:\n%s", out)
	}
}

// A service.env write failure must be loud but must not look like the install
// itself failed — by then the unit is written and the service is running.
func TestInstallReport_SurfacesAMergeError(t *testing.T) {
	spec := serviceSpec{SecretEnv: map[string]string{"RAFIKI_DB": "postgres://u@h/db"}}
	out := installReport(spec, "/cfg/service.env", paths.MergeResult{}, errors.New("permission denied"))

	if !strings.Contains(out, "permission denied") {
		t.Errorf("report does not surface the write error:\n%s", out)
	}
	if !strings.Contains(out, "RAFIKI_DB") {
		t.Errorf("report does not name what failed to reach the file:\n%s", out)
	}
}
