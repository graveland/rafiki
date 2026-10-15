package paths

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// writeEnvMode writes content to path with an explicit mode, for tests that
// care about the permission bits (unlike writeEnv, which pins 0600).
func writeEnvMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(content), mode), "write fixture")
}

// unsetEnv registers a cleanup-restoring value for each key and then unsets it,
// so LoadEnvFile's fill-gaps precedence sees an empty environment and applies
// the file. (Same shape envfile_test.go uses inline.)
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k) //nolint:errcheck // best-effort
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	assert.NewAborting(t).NoError(err, "read %s", path)
	return string(b)
}

func modeT(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	assert.NewAborting(t).NoError(err, "stat %s", path)
	return fi.Mode().Perm()
}

func TestSetEnvFileCreates0600(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "nested", "service.env")
	unsetEnv(t, "RAFIKI_SET_A", "RAFIKI_SET_B")

	res, err := SetEnvFile(p, map[string]string{"RAFIKI_SET_A": "one", "RAFIKI_SET_B": "two"}, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")
	c.ElementsMatch([]string{"RAFIKI_SET_A", "RAFIKI_SET_B"}, res.Added, "Added")
	c.Empty(res.Replaced, "Replaced")
	c.Empty(res.Unchanged, "Unchanged")
	c.Eq(os.FileMode(0), res.Tightened, "Tightened on create")
	c.Eq(os.FileMode(0o600), modeT(t, p), "created mode")

	applied, _, err := LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.ElementsMatch([]string{"RAFIKI_SET_A", "RAFIKI_SET_B"}, applied, "applied")
	c.Eq("one", os.Getenv("RAFIKI_SET_A"), "A")
	c.Eq("two", os.Getenv("RAFIKI_SET_B"), "B")
}

func TestSetEnvFileReplacesInPlacePreservingOthers(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")
	// parseEnvFile takes an unquoted value verbatim (no inline-comment syntax),
	// so this fixture's `# note` is part of B's old value — which is exactly
	// what must be wiped when B's single line is rewritten.
	original := "# hand\nA=1\n\nB=old # note\nC=3\n"
	writeEnvMode(t, p, original, 0o600)

	res, err := SetEnvFile(p, map[string]string{"B": "new"}, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")
	c.ElementsMatch([]string{"B"}, res.Replaced, "Replaced")
	c.Empty(res.Added, "Added")
	c.Empty(res.Unchanged, "Unchanged")

	c.Eq("# hand\nA=1\n\nB=\"new\"\nC=3\n", readFileT(t, p), "only B's line changed")
}

func TestSetEnvFileFirstDefinitionWins(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")
	writeEnvMode(t, p, "A=1\nA=2\n", 0o600)
	unsetEnv(t, "A")

	res, err := SetEnvFile(p, map[string]string{"A": "3"}, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")
	c.ElementsMatch([]string{"A"}, res.Replaced, "Replaced")

	// The FIRST definition is the one rewritten (LoadEnvFile fills gaps, so it
	// is the one that lands); the later duplicate is left byte-identical. The
	// writer always double-quotes (escapeEnvValue), so the fresh line renders
	// as A="3". This bytes assertion is the guard: rewriting the LAST
	// definition would instead yield A=1\nA="3"\n.
	c.Eq("A=\"3\"\nA=2\n", readFileT(t, p), "first definition rewritten")

	unsetEnv(t, "A")
	_, _, err = LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.Eq("3", os.Getenv("A"), "A read back after LoadEnvFile")
}

func TestSetEnvFileUnchangedDoesNotWrite(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")
	writeEnvMode(t, p, "A=1\n", 0o644)
	old := time.Unix(1000000, 0)
	c.Require().NoError(os.Chtimes(p, old, old), "Chtimes")

	res, err := SetEnvFile(p, map[string]string{"A": "1"}, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")
	c.ElementsMatch([]string{"A"}, res.Unchanged, "Unchanged")
	c.Empty(res.Added, "Added")
	c.Empty(res.Replaced, "Replaced")
	c.Eq(os.FileMode(0), res.Tightened, "Tightened")
	c.NotEmpty(res.Warnings, "expected a loose-permission warning")

	fi, err := os.Stat(p)
	c.Require().NoError(err, "Stat")
	c.True(fi.ModTime().Equal(old), "mtime unchanged: got %v want %v", fi.ModTime(), old)
	c.Eq(os.FileMode(0o644), fi.Mode().Perm(), "mode left alone")
	c.Eq("A=1\n", readFileT(t, p), "bytes untouched")
}

func TestSetEnvFileTightensBeforeWrite(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")
	writeEnvMode(t, p, "A=1\n", 0o644)

	res, err := SetEnvFile(p, map[string]string{"B": "2"}, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")
	c.ElementsMatch([]string{"B"}, res.Added, "Added")
	c.Eq(os.FileMode(0o644), res.Tightened, "Tightened")
	c.Eq(os.FileMode(0o600), modeT(t, p), "mode tightened")

	got := readFileT(t, p)
	c.StrContains(got, "A=1\n", "existing line preserved")
	c.StrContains(got, "B=\"2\"\n", "new key appended")
}

func TestSetEnvFileRoundTripsHardValues(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")
	want := map[string]string{
		"RAFIKI_SET_DSN":  `postgres://u@h:5432/db?sslmode=disable&application_name=x`,
		"RAFIKI_SET_NL":   "first\nsecond",
		"RAFIKI_SET_HASH": `say "hi" # now`,
	}
	unsetEnv(t, "RAFIKI_SET_DSN", "RAFIKI_SET_NL", "RAFIKI_SET_HASH")

	_, err := SetEnvFile(p, want, "rafiki setup")
	c.Require().NoError(err, "SetEnvFile")

	applied, warnings, err := LoadEnvFile(p)
	c.Require().NoError(err, "LoadEnvFile")
	c.Empty(warnings, "unexpected warnings")
	c.ElementsMatch([]string{"RAFIKI_SET_DSN", "RAFIKI_SET_NL", "RAFIKI_SET_HASH"}, applied, "applied")
	for k, v := range want {
		c.Eq(v, os.Getenv(k), "%s = %q, want", k, os.Getenv(k))
	}
}

func TestSetEnvFileRejectsEmptyValue(t *testing.T) {
	c := assert.NewCollecting(t)
	p := filepath.Join(t.TempDir(), "service.env")

	_, err := SetEnvFile(p, map[string]string{"A": ""}, "rafiki setup")
	c.ErrorContains(err, "SetEnvFile: empty value for A", "empty value")
	c.NoFileExists(p, "no file written for a rejected call")
}
