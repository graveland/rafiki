//go:build linux

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/rafikid",
		HomeEnv:      "/home/testuser",
		LogPath:      "/home/testuser/.local/state/rafiki/controller.log",
		PathEnv:      "/usr/local/bin:/usr/bin:/bin",
	}
}

func TestRenderUnit_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/rafikid",
		"/home/testuser",
		"/usr/local/bin:/usr/bin:/bin",
		"controller.log",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("unit file missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestRenderUnit_Format(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	checks := []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"Description=rafiki daemon",
		"After=default.target",
		"Restart=on-failure",
		"WantedBy=default.target",
		"Environment=HOME=",
		"Environment=PATH=",
	}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("unit file missing %q", c)
		}
	}
}

func TestRenderUnit_IncludesCapturedEnv(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_DB": "postgres://postgres@localhost:5432/rafiki?sslmode=disable",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	want := "Environment=RAFIKI_DB=postgres://postgres@localhost:5432/rafiki?sslmode=disable"
	if !strings.Contains(out, want) {
		t.Errorf("unit missing %q\n---\n%s", want, out)
	}
}

// systemd splits an unquoted Environment= assignment on whitespace, so a value
// containing a space would truncate at the first one and the remainder would be
// parsed as a second malformed assignment.
func TestRenderUnit_QuotesValuesWithSpaces(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{"RAFIKI_INSTRUCTIONS": "/home/u/My Docs/instructions.md"}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	want := `Environment="RAFIKI_INSTRUCTIONS=/home/u/My Docs/instructions.md"`
	if !strings.Contains(out, want) {
		t.Errorf("unit missing quoted assignment %q\n---\n%s", want, out)
	}
}

func TestUnitQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"K=simple", "K=simple"},
		{"K=with space", `"K=with space"`},
		{`K=has"quote`, `"K=has\"quote"`},
		{`K=has\backslash`, `"K=has\\backslash"`},
	} {
		if got := unitQuote(tc.in); got != tc.want {
			t.Errorf("unitQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUnitPath_UsesSystemdUnitName is the regression test for the one hole a
// reviewer found: nothing checked that unitPath() or the systemctl commands
// actually key off systemdUnitName. Reverting the constant used to fail no
// test at all — the daemon-facing unit file could drift from what
// `systemctl` targets with nothing catching it.
func TestUnitPath_UsesSystemdUnitName(t *testing.T) {
	b := &linuxBackend{}
	got, err := b.unitPath()
	if err != nil {
		t.Fatalf("unitPath: %v", err)
	}
	want := systemdUnitName + ".service"
	if filepath.Base(got) != want {
		t.Errorf("unitPath() = %q, want a path ending in %q", got, want)
	}
}

// TestSystemctlCommands_UseSystemdUnitName asserts every systemctl
// invocation a linuxBackend method issues names the unit via the
// systemdUnitName constant, by stubbing runOSCmd and inspecting the args.
func TestSystemctlCommands_UseSystemdUnitName(t *testing.T) {
	orig := runOSCmd
	defer func() { runOSCmd = orig }()

	var calls [][]string
	runOSCmd = func(name string, args ...string) (string, error) {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		return "", nil
	}

	// Status() is deliberately excluded: it short-circuits on os.Stat(unitPath)
	// before ever calling runOSCmd, so whether it contributes a call depends on
	// whether this machine happens to have the unit file installed for real.
	b := &linuxBackend{}
	_ = b.Start()
	_ = b.Stop()
	_ = b.Restart()

	if len(calls) == 0 {
		t.Fatal("no systemctl commands were issued")
	}
	for _, call := range calls {
		found := false
		for _, arg := range call {
			if arg == systemdUnitName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("systemctl call %v does not reference systemdUnitName %q", call, systemdUnitName)
		}
	}
}

func TestRenderUnit_Deterministic(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{"RAFIKI_DB": "db", "RAFIKI_SOCKET": "/s", "RAFIKI_PI_BINARY": "/pi"}
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
			t.Fatal("unit rendering is not deterministic")
		}
	}
}
