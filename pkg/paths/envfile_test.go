package paths

import (
	"os"
	"path/filepath"
	"slices"
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

// parseEnvFile is the pure half of LoadEnvFile: it must report what a file
// says without touching the process environment. MergeEnvFile depends on
// that, because it has to learn a file's keys during an install without
// applying anyone's credentials to the running command.
func TestParseEnvFile_DoesNotTouchTheEnvironment(t *testing.T) {
	const key = "RAFIKI_PARSE_PURITY_PROBE"
	if _, ok := os.LookupEnv(key); ok {
		t.Fatalf("%s is already set; pick a different probe name", key)
	}

	vars, warnings, err := parseEnvFile(strings.NewReader(key+"=value\n"), "probe.env")
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(vars) != 1 || vars[0].Key != key || vars[0].Value != "value" {
		t.Fatalf("vars = %+v, want one %s=value", vars, key)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Errorf("parseEnvFile exported %s into the environment", key)
	}
}

// File order is preserved: MergeEnvFile reports a file's keys, and a shuffled
// report is a confusing one.
func TestParseEnvFile_PreservesOrder(t *testing.T) {
	vars, _, err := parseEnvFile(strings.NewReader("B=2\nA=1\nC=3\n"), "probe.env")
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	var got []string
	for _, v := range vars {
		got = append(got, v.Key)
	}
	if !slices.Equal(got, []string{"B", "A", "C"}) {
		t.Errorf("got %v, want [B A C] in file order", got)
	}
}

// The property the whole feature rests on: whatever MergeEnvFile writes,
// parseEnvFile must read back byte-identically. A DSN carries '?' and '&',
// a password can carry anything at all.
func TestMergeEnvFile_RoundTripsAwkwardValues(t *testing.T) {
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
	if err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	if len(res.Added) != len(awkward) {
		t.Fatalf("Added = %v, want all %d keys", res.Added, len(awkward))
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, warnings, err := parseEnvFile(f, path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("re-parsing what we wrote produced warnings: %v", warnings)
	}
	back := map[string]string{}
	for _, v := range got {
		back[v.Key] = v.Value
	}
	for k, want := range awkward {
		if back[k] != want {
			t.Errorf("%s round-tripped as %q, want %q", k, back[k], want)
		}
	}
}

// Append-only is what makes this safe against a hand-maintained file. An
// existing key is never rewritten, and a differing value is reported rather
// than silently discarded or silently overwritten.
func TestMergeEnvFile_NeverRewritesAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	original := "# hand written\nRAFIKI_DB=postgres://old@h/db\nRAFIKI_SAMPLE_SECRET=same\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := MergeEnvFile(path, map[string]string{
		"RAFIKI_DB":          "postgres://new@h/db", // differs -> Conflict
		"RAFIKI_SAMPLE_SECRET": "same",                // identical -> Existing
		"ANTHROPIC_API_KEY":  "sk-ant",              // new -> Added
	}, "test")
	if err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	if !slices.Equal(res.Added, []string{"ANTHROPIC_API_KEY"}) {
		t.Errorf("Added = %v, want [ANTHROPIC_API_KEY]", res.Added)
	}
	if !slices.Equal(res.Existing, []string{"RAFIKI_SAMPLE_SECRET"}) {
		t.Errorf("Existing = %v, want [RAFIKI_SAMPLE_SECRET]", res.Existing)
	}
	if !slices.Equal(res.Conflict, []string{"RAFIKI_DB"}) {
		t.Errorf("Conflict = %v, want [RAFIKI_DB]", res.Conflict)
	}
	if !slices.Equal(res.Defined, []string{"RAFIKI_DB", "RAFIKI_SAMPLE_SECRET"}) {
		t.Errorf("Defined = %v, want the file's pre-merge keys in order", res.Defined)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), original) {
		t.Error("MergeEnvFile rewrote existing content instead of appending to it")
	}
	if strings.Contains(string(after), "postgres://new@h/db") {
		t.Error("a conflicting value was written into the file")
	}
}

// A reinstall that has nothing new to add must not touch the file at all —
// otherwise repeated installs accumulate empty comment headers.
func TestMergeEnvFile_NoOpLeavesTheFileByteIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	vars := map[string]string{"RAFIKI_DB": "postgres://u@h/db"}
	if _, err := MergeEnvFile(path, vars, "test"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := MergeEnvFile(path, vars, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Errorf("Added = %v on a no-op merge, want none", res.Added)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a no-op merge changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// It holds credentials. A file this creates must not be readable by anyone else.
func TestMergeEnvFile_CreatesAt0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "service.env")
	if _, err := MergeEnvFile(path, map[string]string{"K": "v"}, "test"); err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("created mode %04o, want 0600", perm)
	}
}

// Appending a NEW credential into an existing loose-permission file tightens
// it to 0600 first and reports having done so via MergeResult.Tightened —
// this is the reversed decision: there is a difference between OBSERVING a
// loose-permission file and actively ADDING a new credential to it, and once
// a credential is about to be appended, leaving the file world-readable would
// defeat the reason it exists.
func TestMergeEnvFile_TightensLoosePermissionsBeforeAppendingACredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("EXISTING=1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := MergeEnvFile(path, map[string]string{"NEW": "v"}, "test")
	if err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	if res.Tightened != 0644 {
		t.Errorf("Tightened = %04o, want 0644 (the permissions it tightened FROM)", res.Tightened)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %04o after appending a credential, want 0600", perm)
	}
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
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("EXISTING=1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := MergeEnvFile(path, map[string]string{"EXISTING": "1"}, "test") // already present, identical value
	if err != nil {
		t.Fatalf("MergeEnvFile: %v", err)
	}
	if len(res.Added) != 0 {
		t.Fatalf("Added = %v, want none (this is a pure observe, nothing new)", res.Added)
	}
	if res.Tightened != 0 {
		t.Errorf("Tightened = %04o, want 0: nothing was added, so permissions must be left alone", res.Tightened)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("no warning for a 0644 file holding credentials")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0644 {
		t.Errorf("mode changed to %04o; an observe-only merge must not chmod the file", perm)
	}
}

// A file not ending in a newline must not have the first appended line
// concatenated onto its last one.
func TestMergeEnvFile_SeparatesFromAnUnterminatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("EXISTING=1"), 0600); err != nil { // no trailing \n
		t.Fatal(err)
	}
	if _, err := MergeEnvFile(path, map[string]string{"NEW": "v"}, "test"); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vars, _, err := parseEnvFile(f, path)
	if err != nil {
		t.Fatal(err)
	}
	back := map[string]string{}
	for _, v := range vars {
		back[v.Key] = v.Value
	}
	if back["EXISTING"] != "1" || back["NEW"] != "v" {
		t.Errorf("appending to an unterminated file corrupted it: %v", back)
	}
}
