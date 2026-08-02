package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadEnvFile_Basics(t *testing.T) {
	p := writeEnv(t, `# a comment

RAFIKI_TEST_BARE=plain
export RAFIKI_TEST_EXPORTED=exported
RAFIKI_TEST_DQ="double quoted"
RAFIKI_TEST_SQ='single quoted'
RAFIKI_TEST_EMPTY=
RAFIKI_TEST_DSN=postgres://u@h:5432/db?sslmode=disable&application_name=fundi
`)
	for _, k := range []string{"RAFIKI_TEST_BARE", "RAFIKI_TEST_EXPORTED", "RAFIKI_TEST_DQ", "RAFIKI_TEST_SQ", "RAFIKI_TEST_EMPTY", "RAFIKI_TEST_DSN"} {
		t.Setenv(k, "") // registers cleanup; unset below so the file applies
		os.Unsetenv(k)  //nolint:errcheck // best-effort
	}

	applied, warnings, err := LoadEnvFile(p)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(applied) != 6 {
		t.Errorf("applied %d vars, want 6: %v", len(applied), applied)
	}
	for k, want := range map[string]string{
		"RAFIKI_TEST_BARE":     "plain",
		"RAFIKI_TEST_EXPORTED": "exported",
		"RAFIKI_TEST_DQ":       "double quoted",
		"RAFIKI_TEST_SQ":       "single quoted",
		"RAFIKI_TEST_EMPTY":    "",
		// An unquoted DSN keeps its query string verbatim — no shell-style
		// expansion, so & and ? are ordinary characters.
		"RAFIKI_TEST_DSN": "postgres://u@h:5432/db?sslmode=disable&application_name=fundi",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// The whole reason this file exists: ANTHROPIC_CUSTOM_HEADERS accepts a literal
// newline and nothing else as its separator, and a systemd unit cannot carry
// one. Both spellings must produce a real newline.
func TestLoadEnvFile_Newlines(t *testing.T) {
	t.Run("escape sequence", func(t *testing.T) {
		p := writeEnv(t, "RAFIKI_TEST_HDRS=\"X-A: 1\\nX-B: 2\"\n")
		os.Unsetenv("RAFIKI_TEST_HDRS") //nolint:errcheck
		t.Setenv("RAFIKI_TEST_HDRS", "")
		os.Unsetenv("RAFIKI_TEST_HDRS") //nolint:errcheck
		if _, _, err := LoadEnvFile(p); err != nil {
			t.Fatalf("LoadEnvFile: %v", err)
		}
		if got := os.Getenv("RAFIKI_TEST_HDRS"); got != "X-A: 1\nX-B: 2" {
			t.Errorf("got %q, want a real newline between the headers", got)
		}
	})

	t.Run("literal multi-line value", func(t *testing.T) {
		p := writeEnv(t, "RAFIKI_TEST_HDRS2=\"X-A: 1\nX-B: 2\"\nRAFIKI_TEST_AFTER=after\n")
		t.Setenv("RAFIKI_TEST_HDRS2", "")
		os.Unsetenv("RAFIKI_TEST_HDRS2") //nolint:errcheck
		t.Setenv("RAFIKI_TEST_AFTER", "")
		os.Unsetenv("RAFIKI_TEST_AFTER") //nolint:errcheck
		if _, _, err := LoadEnvFile(p); err != nil {
			t.Fatalf("LoadEnvFile: %v", err)
		}
		if got := os.Getenv("RAFIKI_TEST_HDRS2"); got != "X-A: 1\nX-B: 2" {
			t.Errorf("got %q, want the two lines joined by a newline", got)
		}
		// Parsing must resume normally after the multi-line value closes.
		if got := os.Getenv("RAFIKI_TEST_AFTER"); got != "after" {
			t.Errorf("assignment after a multi-line value was lost: %q", got)
		}
	})
}

// The real environment wins, so `RAFIKI_DB=... rafikid` still overrides the
// file and a service manager's own settings are not silently replaced.
func TestLoadEnvFile_ExistingEnvWins(t *testing.T) {
	p := writeEnv(t, "RAFIKI_TEST_PRESET=from-file\n")
	t.Setenv("RAFIKI_TEST_PRESET", "from-environment")

	applied, _, err := LoadEnvFile(p)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("RAFIKI_TEST_PRESET"); got != "from-environment" {
		t.Errorf("file overrode the real environment: got %q", got)
	}
	for _, k := range applied {
		if k == "RAFIKI_TEST_PRESET" {
			t.Error("reported applying a variable that was already set")
		}
	}
}

// Optional configuration: a daemon that refuses to start because an optional
// file is absent is worse than one that starts without it.
func TestLoadEnvFile_MissingIsNotAnError(t *testing.T) {
	applied, warnings, err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil || len(applied) != 0 || len(warnings) != 0 {
		t.Errorf("missing file: got applied=%v warnings=%v err=%v, want all empty", applied, warnings, err)
	}
}

// One bad line must not cost the operator every good one — the failure mode
// pkg/models' silent nil-on-parse-error demonstrates.
func TestLoadEnvFile_MalformedLinesWarnButDoNotAbort(t *testing.T) {
	p := writeEnv(t, "this is not an assignment\n=novalue\nRAFIKI_TEST_GOOD=kept\n")
	t.Setenv("RAFIKI_TEST_GOOD", "")
	os.Unsetenv("RAFIKI_TEST_GOOD") //nolint:errcheck

	applied, warnings, err := LoadEnvFile(p)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if len(warnings) != 2 {
		t.Errorf("got %d warnings, want 2: %v", len(warnings), warnings)
	}
	if os.Getenv("RAFIKI_TEST_GOOD") != "kept" {
		t.Error("a good assignment after malformed lines was dropped")
	}
	if len(applied) != 1 {
		t.Errorf("applied = %v, want just the good one", applied)
	}
}

func TestLoadEnvFile_WarnsOnLoosePermissions(t *testing.T) {
	p := writeEnv(t, "RAFIKI_TEST_PERM=x\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_TEST_PERM", "")
	os.Unsetenv("RAFIKI_TEST_PERM") //nolint:errcheck

	applied, warnings, err := LoadEnvFile(p)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "0600") {
		t.Errorf("want a permissions warning naming 0600, got %v", warnings)
	}
	// Warn, do not refuse: the variables must still be applied.
	if os.Getenv("RAFIKI_TEST_PERM") != "x" || len(applied) != 1 {
		t.Error("loose permissions blocked the load; it should only warn")
	}
}

func TestServiceEnvFile_HonoursOverride(t *testing.T) {
	t.Setenv(EnvFile, "/tmp/custom.env")
	if got := ServiceEnvFile(); got != "/tmp/custom.env" {
		t.Errorf("ServiceEnvFile() = %q, want the override", got)
	}
	t.Setenv(EnvFile, "")
	if got := ServiceEnvFile(); filepath.Base(got) != "service.env" {
		t.Errorf("ServiceEnvFile() = %q, want it to end in service.env", got)
	}
}
