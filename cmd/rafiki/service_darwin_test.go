//go:build darwin

package main

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/rafikid",
		HomeEnv:      "/Users/testuser",
		LogPath:      "/Users/testuser/.local/state/rafiki/controller.log",
		PathEnv:      "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func TestRenderPlist_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/rafikid",
		"/Users/testuser",
		"/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"controller.log",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestRenderPlist_Format(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	checks := []string{
		"<?xml version=\"1.0\"",
		"<plist version=\"1.0\">",
		"dev.graveland.rafiki",
		"<key>Label</key>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
		"<key>EnvironmentVariables</key>",
		"<key>HOME</key>",
		"<key>PATH</key>",
	}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("plist missing %q", c)
		}
	}
}

func TestRenderPlist_IncludesCapturedEnv(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_URL":    "postgres://postgres@localhost:5432/rafiki?sslmode=disable",
		"RAFIKI_SOCKET": "/tmp/rafiki.sock",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for _, want := range []string{
		"<key>RAFIKI_URL</key>",
		"<string>postgres://postgres@localhost:5432/rafiki?sslmode=disable</string>",
		"<key>RAFIKI_SOCKET</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n---\n%s", want, out)
		}
	}
}

// A unit value routinely carries a query string, so an unescaped ampersand
// would produce a plist launchd refuses to parse — the service would become
// uninstallable over the very variable this mechanism exists to carry.
func TestRenderPlist_EscapesXML(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_URL": "postgres://h/db?sslmode=disable&application_name=rafiki<1>",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	if strings.Contains(out, "&application_name") {
		t.Error("raw ampersand emitted; the plist would not parse")
	}
	if !strings.Contains(out, "&amp;application_name") {
		t.Errorf("ampersand not escaped\n---\n%s", out)
	}
	if strings.Contains(out, "rafiki<1>") {
		t.Error("raw angle brackets emitted")
	}
	// The real check: it must actually parse as XML.
	if err := xml.Unmarshal([]byte(out), new(any)); err != nil {
		t.Errorf("rendered plist is not well-formed XML: %v\n---\n%s", err, out)
	}
}

// Reinstalling with an unchanged environment must produce a byte-identical
// plist; a map ranged directly would reorder keys at random and make every
// reinstall look like a change.
func TestRenderPlist_Deterministic(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_URL": "url", "RAFIKI_SOCKET": "/s", "RAFIKI_PI_BINARY": "/pi",
		"RAFIKI_DEFAULT_MODEL": "m", "RAFIKI_MCP_CONFIG": "/c",
	}
	first, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for range 20 {
		again, err := renderServiceConfig(spec)
		if err != nil {
			t.Fatalf("renderServiceConfig: %v", err)
		}
		if again != first {
			t.Fatal("plist rendering is not deterministic")
		}
	}
}

// The plist is 0644. Whatever else changes, a DSN must never appear in it.
func TestRenderServiceConfig_NeverContainsADSN(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{
		"RAFIKI_DB=postgres://u:hunter2@localhost/rafiki",
		"RAFIKI_DEFAULT_MODEL=anthropic/opus-latest",
	})
	out, err := renderServiceConfig(serviceSpec{
		DaemonBinary: "/usr/local/bin/rafikid",
		PathEnv:      "/usr/bin",
		HomeEnv:      "/Users/test",
		LogPath:      "/tmp/rafiki.log",
		ExtraEnv:     unit,
		SecretEnv:    secret,
	})
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for _, forbidden := range []string{"hunter2", "postgres://", "RAFIKI_DB"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered plist contains %q:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "RAFIKI_DEFAULT_MODEL") {
		t.Error("a non-secret variable stopped being baked into the plist")
	}
}

// No captured environment must still render a valid plist — the pre-existing
// HOME/PATH-only shape.
func TestRenderPlist_EmptyExtraEnv(t *testing.T) {
	out, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	if strings.Contains(out, "RAFIKI_") {
		t.Errorf("unexpected RAFIKI_ key with no captured env\n---\n%s", out)
	}
	if err := xml.Unmarshal([]byte(out), new(any)); err != nil {
		t.Errorf("not well-formed XML: %v", err)
	}
}

// The regression this guards: `launchctl bootstrap` fails against a job that
// is already loaded (a reinstall), and in that failure it does NOT replace
// the running job's definition — the legacy `load` fallback is equally a
// no-op against an already-loaded job. Verified on a real machine: without a
// bootout first, the plist on disk showed a fresh definition while
// `launchctl print` on the live job still showed the OLD one. Install must
// bootout (best-effort) before bootstrap so a reinstall actually replaces the
// running job, not just the file on disk.
func TestDarwinInstall_BootsOutBeforeBootstrappingSoAReinstallActuallyReloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	sleepFn = func(time.Duration) {}
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()

	var calls [][]string
	runOSCmd = func(name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		// Every call, including `print`, reports success/loaded: bootstrap
		// succeeds, the legacy load fallback is never reached, and the
		// post-install verification print sees the job present.
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")

	if err := b.Install(spec); err != nil {
		t.Fatalf("Install: %v", err)
	}

	var bootoutIdx, bootstrapIdx = -1, -1
	for i, call := range calls {
		if len(call) < 2 {
			continue
		}
		switch call[1] {
		case "bootout":
			bootoutIdx = i
		case "bootstrap":
			bootstrapIdx = i
		}
	}
	if bootoutIdx == -1 {
		t.Fatalf("Install never called launchctl bootout; calls: %v", calls)
	}
	if bootstrapIdx == -1 {
		t.Fatalf("Install never called launchctl bootstrap; calls: %v", calls)
	}
	if bootoutIdx >= bootstrapIdx {
		t.Errorf("bootout (call %d) did not run before bootstrap (call %d): %v", bootoutIdx, bootstrapIdx, calls)
	}
}

// Install must still succeed when bootout fails — the expected, unproblematic
// case on a clean machine where the job was never loaded in the first place.
func TestDarwinInstall_SucceedsWhenBootoutFailsBecauseNotLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	sleepFn = func(time.Duration) {}
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()
	var printCallsBeforeBootstrap int
	var sawBootstrap bool
	runOSCmd = func(_ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "bootout" {
			return "Could not find service", errors.New("exit status 3")
		}
		if len(args) > 0 && args[0] == "bootstrap" {
			sawBootstrap = true
		}
		if len(args) > 0 && args[0] == "print" && !sawBootstrap {
			printCallsBeforeBootstrap++
		}
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")
	if err := b.Install(spec); err != nil {
		t.Fatalf("Install: %v, want success even though bootout failed (job was never loaded)", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")); err != nil {
		t.Errorf("plist was not written: %v", err)
	}
	if printCallsBeforeBootstrap != 0 {
		t.Errorf("expected the poll to be skipped on the bootout-not-found fast path, but launchctl print was called %d time(s) before bootstrap", printCallsBeforeBootstrap)
	}
}

// The regression this guards, verified twice on real hardware: bootout is
// async and returns before the old job is gone; bootstrap then races it and
// fails; the legacy `load` fallback returns exit 0 without loading anything;
// and a naive Install would return nil while `launchctl list` shows the
// service completely unloaded and the daemon dead. Install must not trust
// bootstrap/load exit codes — it must verify via `launchctl print` and
// return a real error when that verification shows the job never loaded.
func TestDarwinInstall_LyingLoadExitZeroStillReportsErrorWhenVerificationFindsNothingLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	sleepFn = func(time.Duration) {}
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()
	runOSCmd = func(_ string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "bootout":
			return "Could not find service", errors.New("exit status 3")
		case "bootstrap":
			return "service already loaded", errors.New("exit status 1")
		case "load":
			// The lying fallback: exit 0 without having loaded anything.
			return "", nil
		case "print":
			return "Could not find service", errors.New("exit status 3")
		}
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")
	err := b.Install(spec)
	if err == nil {
		t.Fatal("Install: got nil error, want a real error — the service verifiably did not load")
	}
	if !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Install error %q does not clearly say the service is not loaded", err.Error())
	}
}

// bootout succeeding means a job really was loaded and is (asynchronously)
// on its way out. Install must poll `launchctl print` until it reports gone
// before bootstrapping, and stop polling as soon as it does — it must not
// always burn the full cap.
func TestDarwinInstall_PollsUntilUnloadedThenBootstraps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	var sleeps int
	sleepFn = func(time.Duration) { sleeps++ }
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()

	var printCalls, printCallsBeforeBootstrap int
	var sawBootstrap bool
	runOSCmd = func(_ string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "bootout":
			return "", nil // succeeds: the job was loaded
		case "bootstrap":
			sawBootstrap = true
			return "", nil
		case "print":
			printCalls++
			if !sawBootstrap {
				printCallsBeforeBootstrap = printCalls
			}
			// Still loaded for the first two polls, gone on the third —
			// and loaded again once bootstrap has run (post-install
			// verification).
			if printCalls <= 2 {
				return "", nil
			}
			if printCalls == 3 {
				return "Could not find service", errors.New("exit status 3")
			}
			return "", nil
		}
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")
	if err := b.Install(spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !sawBootstrap {
		t.Fatal("Install never called launchctl bootstrap")
	}
	if printCallsBeforeBootstrap != 3 {
		t.Errorf("expected the poll to stop as soon as print reported gone (3rd call), got %d print calls before bootstrap", printCallsBeforeBootstrap)
	}
	if sleeps == 0 {
		t.Error("expected the poll to sleep between attempts")
	}
}

// If the job never reports gone, Install must not give up on reloading the
// machine: it still proceeds to bootstrap once the poll cap is exhausted.
func TestDarwinInstall_PollCapExpiryStillAttemptsBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	var sleeps int
	sleepFn = func(time.Duration) { sleeps++ }
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()

	var printCallsBeforeBootstrap int
	var sawBootstrap bool
	runOSCmd = func(_ string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "bootout":
			return "", nil // succeeds: the job was loaded
		case "bootstrap":
			sawBootstrap = true
			return "", nil
		case "print":
			if !sawBootstrap {
				printCallsBeforeBootstrap++
			}
			return "", nil // never reports gone
		}
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")
	if err := b.Install(spec); err != nil {
		t.Fatalf("Install: %v, want the cap expiring to still fall through to bootstrap and succeed", err)
	}
	if !sawBootstrap {
		t.Fatal("Install never called launchctl bootstrap after the poll cap expired")
	}
	wantAttempts := int(installPollCap / installPollInterval)
	if printCallsBeforeBootstrap != wantAttempts {
		t.Errorf("expected the poll to exhaust its full cap of %d attempts, got %d", wantAttempts, printCallsBeforeBootstrap)
	}
	if sleeps != wantAttempts {
		t.Errorf("expected %d sleeps (one per poll attempt), got %d", wantAttempts, sleeps)
	}
}

// Controller-review regression: bootout succeeds (a job WAS loaded) but the
// poll cap expires without ever confirming it gone; bootstrap then fails
// (because the stale job is still bootstrapped); the legacy load fallback
// lies with exit 0; and launchctl print afterward reports something loaded.
// That "loaded" cannot be trusted -- it may be the fresh plist, or it may be
// the exact stale job bootout exists to replace, still running under its
// old environment. Since the poll never confirmed the old job was gone AND
// bootstrap itself failed (we fell back to the fallback that lies), Install
// must not report success here.
func TestDarwinInstall_UnconfirmedUnloadWithFailedBootstrapIsAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origSleep := sleepFn
	sleepFn = func(time.Duration) {}
	defer func() { sleepFn = origSleep }()

	orig := runOSCmd
	defer func() { runOSCmd = orig }()
	runOSCmd = func(_ string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "bootout":
			return "", nil // succeeds: a job WAS loaded
		case "bootstrap":
			return "service already bootstrapped", errors.New("exit status 1")
		case "load":
			// The lying fallback: exit 0 without having (re)loaded anything.
			return "", nil
		case "print":
			// Always reports loaded: the poll never confirms the old job
			// gone, and the post-install verification also sees "loaded" --
			// indistinguishable from the stale job still being the live one.
			return "", nil
		}
		return "", nil
	}

	b := &darwinBackend{}
	spec := testSpec()
	spec.LogPath = filepath.Join(home, "controller.log")
	err := b.Install(spec)
	if err == nil {
		t.Fatal("Install: got nil error, want a real error -- the poll never confirmed the stale job was gone, bootstrap failed, and the lying legacy load cannot be trusted to have replaced it")
	}
}
