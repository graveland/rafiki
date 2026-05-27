package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"git.graveland.dev/brent/pi-controller/cmd/pic/picembed"
)

func TestPicHelpers_EmbedHasExpectedFiles(t *testing.T) {
	var seen []string
	if err := fs.WalkDir(picembed.PicHelpers, "pic-helpers", func(path string, d fs.DirEntry, err error) error {
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
		"pic-helpers/package.json",
		"pic-helpers/index.ts",
		"pic-helpers/README.md",
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

func TestEnsurePicHelpersInstalled_FreshInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIC_NO_AUTO_INSTALL_HELPERS", "")
	if err := ensurePicHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".pi", "agent", "extensions", "pic-helpers")
	if _, err := os.Stat(filepath.Join(dest, "index.ts")); err != nil {
		t.Fatalf("index.ts not installed: %v", err)
	}
}

func TestEnsurePicHelpersInstalled_OptOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIC_NO_AUTO_INSTALL_HELPERS", "1")
	if err := ensurePicHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".pi", "agent", "extensions", "pic-helpers")
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("opt-out should have skipped install; got: %v", err)
	}
}

func TestEnsurePicHelpersInstalled_AlreadyCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIC_NO_AUTO_INSTALL_HELPERS", "")
	// First install.
	if err := ensurePicHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	// Stamp a marker file inside to detect overwrite.
	dest := filepath.Join(home, ".pi", "agent", "extensions", "pic-helpers")
	marker := filepath.Join(dest, ".not-overwritten")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second call should be a no-op.
	if err := ensurePicHelpersInstalled(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker missing — install was not idempotent: %v", err)
	}
}
