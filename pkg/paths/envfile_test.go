package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "service.env")
	assert.NewAborting(t).NoError(os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestLoadEnvFile_Basics(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "LoadEnvFile")
	c.Empty(warnings, "unexpected warnings")
	c.Len(applied, 6, "applied %d vars, want 6", len(applied))
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
		got := os.Getenv(k)
		c.Eq(want, got, "%s = %q, want", k, got)
	}
}

// The whole reason this file exists: ANTHROPIC_CUSTOM_HEADERS accepts a literal
// newline and nothing else as its separator, and a systemd unit cannot carry
// one. Both spellings must produce a real newline.
func TestLoadEnvFile_Newlines(t *testing.T) {
	t.Run("escape sequence", func(t *testing.T) {
		c := assert.NewCollecting(t)
		p := writeEnv(t, "RAFIKI_TEST_HDRS=\"X-A: 1\\nX-B: 2\"\n")
		os.Unsetenv("RAFIKI_TEST_HDRS") //nolint:errcheck
		t.Setenv("RAFIKI_TEST_HDRS", "")
		os.Unsetenv("RAFIKI_TEST_HDRS") //nolint:errcheck
		_, _, err := LoadEnvFile(p)
		c.Require().NoError(err, "LoadEnvFile")
		c.Eq("X-A: 1\nX-B: 2", os.Getenv("RAFIKI_TEST_HDRS"), "got")
	})

	t.Run("literal multi-line value", func(t *testing.T) {
		c := assert.NewCollecting(t)
		p := writeEnv(t, "RAFIKI_TEST_HDRS2=\"X-A: 1\nX-B: 2\"\nRAFIKI_TEST_AFTER=after\n")
		t.Setenv("RAFIKI_TEST_HDRS2", "")
		os.Unsetenv("RAFIKI_TEST_HDRS2") //nolint:errcheck
		t.Setenv("RAFIKI_TEST_AFTER", "")
		os.Unsetenv("RAFIKI_TEST_AFTER") //nolint:errcheck
		_, _, err := LoadEnvFile(p)
		c.Require().NoError(err, "LoadEnvFile")
		c.Eq("X-A: 1\nX-B: 2", os.Getenv("RAFIKI_TEST_HDRS2"), "got")
		// Parsing must resume normally after the multi-line value closes.
		c.Eq("after", os.Getenv("RAFIKI_TEST_AFTER"), "assignment after a multi-line value was lost")
	})
}

// The real environment wins, so `RAFIKI_DB=... rafikid` still overrides the
// file and a service manager's own settings are not silently replaced.
func TestLoadEnvFile_ExistingEnvWins(t *testing.T) {
	c := assert.NewCollecting(t)
	p := writeEnv(t, "RAFIKI_TEST_PRESET=from-file\n")
	t.Setenv("RAFIKI_TEST_PRESET", "from-environment")

	applied, _, err := LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.Eq("from-environment", os.Getenv("RAFIKI_TEST_PRESET"), "file overrode the real environment: got")
	for _, k := range applied {
		c.NotEq("RAFIKI_TEST_PRESET", k, "reported applying a variable that was already set")
	}
}

// Optional configuration: a daemon that refuses to start because an optional
// file is absent is worse than one that starts without it.
func TestLoadEnvFile_MissingIsNotAnError(t *testing.T) {
	applied, warnings, err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	assert.NewCollecting(t).False(err != nil || len(applied) != 0 || len(warnings) != 0, "missing file: got applied=%v warnings=%v err=%v, want all empty", applied, warnings, err)
}

// One bad line must not cost the operator every good one — the failure mode
// pkg/models' silent nil-on-parse-error demonstrates.
func TestLoadEnvFile_MalformedLinesWarnButDoNotAbort(t *testing.T) {
	c := assert.NewCollecting(t)
	p := writeEnv(t, "this is not an assignment\n=novalue\nRAFIKI_TEST_GOOD=kept\n")
	t.Setenv("RAFIKI_TEST_GOOD", "")
	os.Unsetenv("RAFIKI_TEST_GOOD") //nolint:errcheck

	applied, warnings, err := LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.Len(warnings, 2, "got %d warnings, want 2", len(warnings))
	c.Eq("kept", os.Getenv("RAFIKI_TEST_GOOD"), "a good assignment after malformed lines was dropped")
	c.Len(applied, 1, "applied")
}

func TestLoadEnvFile_WarnsOnLoosePermissions(t *testing.T) {
	c := assert.NewCollecting(t)
	p := writeEnv(t, "RAFIKI_TEST_PERM=x\n")
	c.Require().NoError(os.Chmod(p, 0o644))
	t.Setenv("RAFIKI_TEST_PERM", "")
	os.Unsetenv("RAFIKI_TEST_PERM") //nolint:errcheck

	applied, warnings, err := LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.False(len(warnings) == 0 || !strings.Contains(warnings[0], "0600"), "want a permissions warning naming 0600, got %v", warnings)
	// Warn, do not refuse: the variables must still be applied.
	c.False(os.Getenv("RAFIKI_TEST_PERM") != "x" || len(applied) != 1, "loose permissions blocked the load; it should only warn")
}

func TestServiceEnvFile_HonoursOverride(t *testing.T) {
	t.Setenv(EnvFile, "/tmp/custom.env")
	c := assert.NewCollecting(t)
	c.Eq("/tmp/custom.env", ServiceEnvFile(), "ServiceEnvFile()")
	t.Setenv(EnvFile, "")
	got := ServiceEnvFile()
	c.Eq("service.env", filepath.Base(got), "ServiceEnvFile() = %q, want it to end in service.env", got)
}

func TestExecutorEnvFile_HonoursOverride(t *testing.T) {
	t.Setenv(ExecutorEnvFileEnv, "/tmp/executor-custom.env")
	c := assert.NewCollecting(t)
	c.Eq("/tmp/executor-custom.env", ExecutorEnvFile(), "ExecutorEnvFile()")
	t.Setenv(ExecutorEnvFileEnv, "")
	got := ExecutorEnvFile()
	c.Eq("executor.env", filepath.Base(got), "ExecutorEnvFile() = %q, want it to end in executor.env", got)
}

func TestExecutorOverridesFile_HonoursOverride(t *testing.T) {
	t.Setenv(ExecutorOverridesFileEnv, "/tmp/executor-overrides-custom.env")
	c := assert.NewCollecting(t)
	c.Eq("/tmp/executor-overrides-custom.env", ExecutorOverridesFile(), "ExecutorOverridesFile()")
	t.Setenv(ExecutorOverridesFileEnv, "")
	got := ExecutorOverridesFile()
	c.Eq("executor-overrides.env", filepath.Base(got), "ExecutorOverridesFile() = %q, want it to end in executor-overrides.env", got)
}

// The overrides loader is the executor's escape hatch: launchd seeds
// SSH_AUTH_SOCK into every LaunchAgent, and under LoadEnvFile's fill-gaps
// precedence a captured value in executor.env is inert — only a file that
// OVERRIDES can beat the unit. Every variable it names wins, present or not,
// which is the exact opposite of TestLoadEnvFile_ExistingEnvWins.
func TestLoadEnvFileOverrides_BeatsTheEnvironment(t *testing.T) {
	c := assert.NewCollecting(t)
	p := writeEnv(t, `RAFIKI_OVR_NEW=from-file
RAFIKI_OVR_EXISTING=from-file
RAFIKI_OVR_EMPTY=
`)
	// Present in the env BEFORE the load, with a different value: the fill-gaps
	// loader would leave it alone; this loader must replace it.
	t.Setenv("RAFIKI_OVR_EXISTING", "from-process")
	os.Unsetenv("RAFIKI_OVR_NEW") //nolint:errcheck
	t.Setenv("RAFIKI_OVR_NEW", "")
	os.Unsetenv("RAFIKI_OVR_NEW") //nolint:errcheck
	// A variable the file does not name must be left untouched.
	t.Setenv("RAFIKI_OVR_UNTOUCHED", "keep-me")

	applied, warnings, err := LoadEnvFileOverrides(p)
	c.Require().NoError(err, "LoadEnvFileOverrides")
	c.Empty(warnings, "unexpected warnings")
	c.Len(applied, 3, "applied %d vars, want 3", len(applied))
	for k, want := range map[string]string{
		"RAFIKI_OVR_NEW":       "from-file",
		"RAFIKI_OVR_EXISTING":  "from-file", // overrode the process value
		"RAFIKI_OVR_EMPTY":     "",
		"RAFIKI_OVR_UNTOUCHED": "keep-me",
	} {
		got := os.Getenv(k)
		c.Eq(want, got, "%s = %q, want", k, got)
	}
}

// Same optional-configuration contract as LoadEnvFile: a missing overrides
// file must not fail serve.
func TestLoadEnvFileOverrides_MissingIsNotAnError(t *testing.T) {
	applied, warnings, err := LoadEnvFileOverrides(filepath.Join(t.TempDir(), "nope.env"))
	assert.NewCollecting(t).False(err != nil || len(applied) != 0 || len(warnings) != 0, "missing file: got applied=%v warnings=%v err=%v, want all empty", applied, warnings, err)
}

// parseEnvFile is the pure half of LoadEnvFile: it must report what a file
// says without touching the process environment. MergeEnvFile depends on
// that, because it has to learn a file's keys during an install without
// applying anyone's credentials to the running command.
func TestParseEnvFile_DoesNotTouchTheEnvironment(t *testing.T) {
	c := assert.NewCollecting(t)
	const key = "RAFIKI_PARSE_PURITY_PROBE"
	if _, ok := os.LookupEnv(key); ok {
		t.Fatalf("%s is already set; pick a different probe name", key)
	}

	vars, warnings, err := parseEnvFile(strings.NewReader(key+"=value\n"), "probe.env")
	c.Require().NoError(err, "parseEnvFile")
	c.Empty(warnings, "warnings")
	c.Require().False(len(vars) != 1 || vars[0].Key != key || vars[0].Value != "value", "vars = %+v, want one %s=value", vars, key)
	_, ok := os.LookupEnv(key)
	c.False(ok, "parseEnvFile exported %s into the environment", key)
}

// File order is preserved: MergeEnvFile reports a file's keys, and a shuffled
// report is a confusing one.
func TestParseEnvFile_PreservesOrder(t *testing.T) {
	c := assert.NewCollecting(t)
	vars, _, err := parseEnvFile(strings.NewReader("B=2\nA=1\nC=3\n"), "probe.env")
	c.Require().NoError(err, "parseEnvFile")
	var got []string
	for _, v := range vars {
		got = append(got, v.Key)
	}
	c.EqDiff([]string{"B", "A", "C"}, got, "got")
}

// The property the whole feature rests on: whatever MergeEnvFile writes,
// parseEnvFile must read back byte-identically. A DSN carries '?' and '&',
// a password can carry anything at all.
func TestMergeEnvFile_RoundTripsAwkwardValues(t *testing.T) {
	c := assert.NewCollecting(t)
	awkward := map[string]string{
		"PLAIN":        "simple",
		"DSN":          "postgres://u:p@h:5432/db?sslmode=disable&application_name=x",
		"HASH":         "value # not a comment",
		"QUOTED":       `he said "hi"`,
		"BACKSLASH":    `C:\path\to\thing`,
		"LEADINGQUOTE": `"starts with a quote`,
		"TRAILINGWS":   "keep this space ",
		"NEWLINE":      "a: 1\nb: 2",
		"EQUALS":       "k=v=w",
	}
	path := filepath.Join(t.TempDir(), "service.env")

	res, err := MergeEnvFile(path, awkward, "test")
	c.Require().NoError(err, "MergeEnvFile")
	c.Require().Len(res.Added, len(awkward), "Added")

	f, err := os.Open(path)
	c.Require().NoError(err)
	defer f.Close()
	got, warnings, err := parseEnvFile(f, path)
	c.Require().NoError(err, "parseEnvFile")
	c.Empty(warnings, "re-parsing what we wrote produced warnings")
	back := map[string]string{}
	for _, v := range got {
		back[v.Key] = v.Value
	}
	for k, want := range awkward {
		c.Eq(want, back[k], "%s round-tripped as %q, want", k, back[k])
	}
}

// Append-only is what makes this safe against a hand-maintained file. An
// existing key is never rewritten, and a differing value is reported rather
// than silently discarded or silently overwritten.
func TestMergeEnvFile_NeverRewritesAnExistingKey(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "service.env")
	original := "# hand written\nRAFIKI_DB=postgres://old@h/db\nRAFIKI_SAMPLE_SECRET=same\n"
	c.Require().NoError(os.WriteFile(path, []byte(original), 0600))

	res, err := MergeEnvFile(path, map[string]string{
		"RAFIKI_DB":            "postgres://new@h/db", // differs -> Conflict
		"RAFIKI_SAMPLE_SECRET": "same",                // identical -> Existing
		"ANTHROPIC_API_KEY":    "sk-ant",              // new -> Added
	}, "test")
	c.Require().NoError(err, "MergeEnvFile")
	c.EqDiff([]string{"ANTHROPIC_API_KEY"}, res.Added, "Added")
	c.EqDiff([]string{"RAFIKI_SAMPLE_SECRET"}, res.Existing, "Existing")
	c.EqDiff([]string{"RAFIKI_DB"}, res.Conflict, "Conflict")
	c.EqDiff([]string{"RAFIKI_DB", "RAFIKI_SAMPLE_SECRET"}, res.Defined, "Defined")

	after, err := os.ReadFile(path)
	c.Require().NoError(err)
	if !strings.HasPrefix(string(after), original) {
		t.Error("MergeEnvFile rewrote existing content instead of appending to it")
	}
	c.NotStrContains(string(after), "postgres://new@h/db", "a conflicting value was written into the file")
}

// A reinstall that has nothing new to add must not touch the file at all —
// otherwise repeated installs accumulate empty comment headers.
func TestMergeEnvFile_NoOpLeavesTheFileByteIdentical(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "service.env")
	vars := map[string]string{"RAFIKI_DB": "postgres://u@h/db"}
	if _, err := MergeEnvFile(path, vars, "test"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	c.Require().NoError(err)

	res, err := MergeEnvFile(path, vars, "test")
	c.Require().NoError(err)
	c.Empty(res.Added, "Added")
	second, err := os.ReadFile(path)
	c.Require().NoError(err)
	c.Eq(string(second), string(first), "a no-op merge changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
}

// It holds credentials. A file this creates must not be readable by anyone else.
func TestMergeEnvFile_CreatesAt0600(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "sub", "service.env")
	if _, err := MergeEnvFile(path, map[string]string{"K": "v"}, "test"); err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	fi, err := os.Stat(path)
	c.Require().NoError(err)
	c.Eq(0600, fi.Mode().Perm(), "created mode")
}

// Appending a NEW credential into an existing loose-permission file tightens
// it to 0600 first and reports having done so via MergeResult.Tightened —
// this is the reversed decision: there is a difference between OBSERVING a
// loose-permission file and actively ADDING a new credential to it, and once
// a credential is about to be appended, leaving the file world-readable would
// defeat the reason it exists.
func TestMergeEnvFile_TightensLoosePermissionsBeforeAppendingACredential(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "service.env")
	c.Require().NoError(os.WriteFile(path, []byte("EXISTING=1\n"), 0644))

	res, err := MergeEnvFile(path, map[string]string{"NEW": "v"}, "test")
	c.Require().NoError(err, "MergeEnvFile")
	c.Eq(0644, res.Tightened, "Tightened")
	fi, err := os.Stat(path)
	c.Require().NoError(err)
	c.Eq(0600, fi.Mode().Perm(), "mode")
	// The credential itself must still have been written.
	if got, err := os.ReadFile(path); err != nil || !strings.Contains(string(got), "NEW=") {
		t.Fatalf("credential was not appended: %v, %q", err, got)
	}
}

// When there is nothing new to append (every key is already Existing or
// Conflict), MergeEnvFile only OBSERVED the file — it must warn about loose
// permissions without touching them, the same as LoadEnvFile's read-side
// warning. Tightening a file's permissions when nothing is actually being
// added to it would be an unannounced side effect on a call that changed
// nothing else.
func TestMergeEnvFile_ObservingALoosePermissionFileWarnsWithoutChmod(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "service.env")
	c.Require().NoError(os.WriteFile(path, []byte("EXISTING=1\n"), 0644))

	res, err := MergeEnvFile(path, map[string]string{"EXISTING": "1"}, "test") // already present, identical value
	c.Require().NoError(err, "MergeEnvFile")
	c.Require().Empty(res.Added, "Added")
	c.Eq(0, res.Tightened, "Tightened")
	c.Require().NotEmpty(res.Warnings, "no warning for a 0644 file holding credentials")
	fi, err := os.Stat(path)
	c.Require().NoError(err)
	c.Eq(0644, fi.Mode().Perm(), "mode changed to")
}

// A file not ending in a newline must not have the first appended line
// concatenated onto its last one.
func TestMergeEnvFile_SeparatesFromAnUnterminatedLastLine(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "service.env")
	c.Require().NoError(os.WriteFile(path, []byte("EXISTING=1"), 0600)) // no trailing \n
	if _, err := MergeEnvFile(path, map[string]string{"NEW": "v"}, "test"); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	c.Require().NoError(err)
	defer f.Close()
	vars, _, err := parseEnvFile(f, path)
	c.Require().NoError(err)
	back := map[string]string{}
	for _, v := range vars {
		back[v.Key] = v.Value
	}
	c.False(back["EXISTING"] != "1" || back["NEW"] != "v", "appending to an unterminated file corrupted it: %v", back)
}
