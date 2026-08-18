package main

import (
	"os"
	"path/filepath"
	"testing"
)

// `rafiki user create` is also the login step: the token is shown once, so
// the CLI must persist it or the user is locked out of their own daemon.
func TestWriteTokenFileCreates0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if err := writeTokenFile(path, "rfk_secret"); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "rfk_secret\n" {
		t.Fatalf("content = %q", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Overwriting must not widen the mode of an existing file, and must not
// leave the old (longer) token's tail behind.
func TestWriteTokenFileOverwritesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("rfk_a_very_long_previous_token\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeTokenFile(path, "rfk_short"); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "rfk_short\n" {
		t.Fatalf("content = %q; the old token was not fully replaced", b)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 after overwrite", fi.Mode().Perm())
	}
}
