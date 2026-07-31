package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/cmd/fundi/helpersembed"
)

func TestHelpers_EmbedHasExpectedFiles(t *testing.T) {
	var seen []string
	if err := fs.WalkDir(helpersembed.Helpers, helpersembed.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		seen = append(seen, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"fundi-helpers/package.json",
		"fundi-helpers/index.ts",
		"fundi-helpers/README.md",
	} {
		found := false
		for _, s := range seen {
			if s == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing embedded file %q; got: %v", expected, seen)
		}
	}
}

func TestInstallFromEmbed_CopiesFiles(t *testing.T) {
	destDir := t.TempDir()
	if err := installFromEmbed(destDir); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"package.json", "index.ts", "README.md"} {
		path := filepath.Join(destDir, expected)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("file %s is empty", path)
		}
	}
}

func TestHelpersVersionCheck_NotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bundled, installed, err := helpersVersionCheck()
	if err != nil {
		t.Fatal(err)
	}
	if bundled == "" {
		t.Fatal("bundled version is empty")
	}
	if installed != "" {
		t.Fatalf("expected installed = \"\", got %q", installed)
	}
}

func TestEnsureHelpersInstalled_FreshInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FUNDI_NO_AUTO_INSTALL_HELPERS", "")
	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".pi", "agent", "extensions", helpersembed.Dir)
	if _, err := os.Stat(filepath.Join(dest, "index.ts")); err != nil {
		t.Fatalf("index.ts not installed: %v", err)
	}
}

func TestEnsureHelpersInstalled_OptOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FUNDI_NO_AUTO_INSTALL_HELPERS", "1")
	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".pi", "agent", "extensions", helpersembed.Dir)
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("opt-out should have skipped install; got: %v", err)
	}
}

func TestEnsureHelpersInstalled_AlreadyCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FUNDI_NO_AUTO_INSTALL_HELPERS", "")
	// First install.
	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	// Stamp a marker file inside to detect overwrite.
	dest := filepath.Join(home, ".pi", "agent", "extensions", helpersembed.Dir)
	marker := filepath.Join(dest, ".not-overwritten")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second call should be a no-op.
	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker missing — install was not idempotent: %v", err)
	}
}

// ─── the pic-helpers → fundi-helpers migration ────────────────────────────────

// writeLegacyHelpers plants a pre-rename pic-helpers install, standing in for
// either fundi's own leftovers or pi-controller's live extension — the whole
// point being that the two are indistinguishable.
func writeLegacyHelpers(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent", "extensions", legacyHelpersDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"@graveland/pic-helpers","version":"0.1.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.ts"), []byte("// theirs, maybe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLegacyHelpers_NeverRemovedOnInstall is the load-bearing guarantee of this
// migration. pi-controller installs an extension with the same name to the same
// path, and nothing distinguishes its copy from ours — so deleting what we find
// risks tearing out a working pi-controller install. One manual step is
// strictly better than that.
func TestLegacyHelpers_NeverRemovedOnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FUNDI_NO_AUTO_INSTALL_HELPERS", "")
	legacy := writeLegacyHelpers(t, home)

	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}

	// The new extension is installed...
	dest := filepath.Join(home, ".pi", "agent", "extensions", helpersembed.Dir)
	if _, err := os.Stat(filepath.Join(dest, "index.ts")); err != nil {
		t.Fatalf("%s not installed: %v", helpersembed.Dir, err)
	}
	// ...and the old one is still there, untouched, contents intact.
	body, err := os.ReadFile(filepath.Join(legacy, "index.ts"))
	if err != nil {
		t.Fatalf("legacy extension was removed or damaged: %v", err)
	}
	if string(body) != "// theirs, maybe" {
		t.Errorf("legacy extension contents changed: %q", body)
	}
}

// TestLegacyHelpers_NotRemovedByRemoveFlag: --remove uninstalls fundi-helpers
// only, for the same reason — a legacy directory may not be ours to delete.
func TestLegacyHelpers_NotRemovedByRemoveFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FUNDI_NO_AUTO_INSTALL_HELPERS", "")
	legacy := writeLegacyHelpers(t, home)
	if err := ensureHelpersInstalled(); err != nil {
		t.Fatal(err)
	}

	cmd := newInstallExtensionCmd()
	cmd.SetArgs([]string{"--remove"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(home, ".pi", "agent", "extensions", helpersembed.Dir)
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--remove did not remove %s: %v", helpersembed.Dir, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("--remove removed the legacy directory, which may be pi-controller's: %v", err)
	}
}

// TestWarnAboutLegacyHelpers_ReportsPathAndCommand: the warning has to be
// actionable, because the user is the one who has to act on it. A vague warning
// leaves them with duplicate slash commands and no idea why.
func TestWarnAboutLegacyHelpers_ReportsPathAndCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := writeLegacyHelpers(t, home)

	out := captureStderr(t, warnAboutLegacyHelpers)

	for _, want := range []string{legacy, "rm -rf", "pi-controller"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not mention %q; got:\n%s", want, out)
		}
	}
}

// TestWarnAboutLegacyHelpers_SilentWhenAbsent: no legacy directory, no noise.
func TestWarnAboutLegacyHelpers_SilentWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if out := captureStderr(t, warnAboutLegacyHelpers); out != "" {
		t.Errorf("warned with no legacy directory present: %q", out)
	}
}

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()

	fn()

	os.Stderr = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}
