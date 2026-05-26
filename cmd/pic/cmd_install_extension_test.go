package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallExtension_LocateSource(t *testing.T) {
	// From the repo root, locatePicHelpersSource should find extensions/pic-helpers
	// via the cwd fallback.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(cwd, "..", "..")
	if err := os.Chdir(repoRoot); err != nil {
		t.Skip("can't chdir to repo root:", err)
	}
	defer os.Chdir(cwd)

	path, err := locatePicHelpersSource()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("expected absolute path, got %q", path)
	}
	info, err := os.Stat(filepath.Join(path, "index.ts"))
	if err != nil {
		t.Fatalf("index.ts not found in %q: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("index.ts is a directory?")
	}
}

func TestInstallExtension_CopyDirThenRemove(t *testing.T) {
	tmpHome := t.TempDir()
	sourceDir := t.TempDir()
	// Make a tiny source structure.
	if err := os.WriteFile(filepath.Join(sourceDir, "index.ts"), []byte("// noop"), 0o644); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(tmpHome, ".pi", "agent", "extensions", "pic-helpers")

	// Use the copyDir helper directly
	if err := os.MkdirAll(filepath.Dir(destDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyDir(sourceDir, destDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "index.ts")); err != nil {
		t.Fatalf("file not copied: %v", err)
	}

	// Remove.
	if err := os.RemoveAll(destDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists: %v", err)
	}
}
