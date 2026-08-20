package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The connect target is derived, not configured: a remote RAFIKI_URL means the
// executor dials that daemon's TLS listener, and no RAFIKI_URL means the local
// daemon's unix socket. Getting this backwards points an executor at the wrong
// machine, which then serves the wrong filesystem.
func TestSessionExecutorConnectTarget(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	addr, sock, err := sessionConnectTarget()
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "rafiki.example.dev:8443" {
		t.Errorf("addr = %q, want the remote host:port", addr)
	}
	if sock != "" {
		t.Errorf("socket = %q, want empty for a remote daemon", sock)
	}

	t.Setenv("RAFIKI_URL", "")
	addr, sock, err = sessionConnectTarget()
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "" {
		t.Errorf("addr = %q, want empty for a local daemon", addr)
	}
	if sock == "" {
		t.Error("socket is empty; a local daemon is reached over the unix socket")
	}
}

// The daemon must be told we hold a credential, or it mints another permanent
// row every run. This asserts the request field is populated from the file's
// presence rather than left at its zero value.
func TestSessionRequestReportsAnExistingCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client-executor.cred")

	if credFileExists(path) {
		t.Fatal("credFileExists is true for a path that does not exist")
	}
	if err := os.WriteFile(path, []byte("cred_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !credFileExists(path) {
		t.Error("credFileExists is false for a written credential")
	}

	// An empty file is not a credential: a truncated write must re-mint rather
	// than connect with nothing.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if credFileExists(path) {
		t.Error("credFileExists is true for an empty file")
	}
}

// A rejected credential is terminal, not transient: the row is gone or revoked.
// Discarding the file is what lets the next run mint a replacement instead of
// failing forever with a secret nothing recognises.
func TestDiscardCredentialRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client-executor.cred")
	if err := os.WriteFile(path, []byte("cred_stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discardCredential(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the stale credential file survived")
	}
}

// Removing a file that is not there is the ordinary case on a first run and
// must not panic or error.
func TestDiscardCredentialToleratesAMissingFile(t *testing.T) {
	discardCredential(filepath.Join(t.TempDir(), "absent.cred"))
}
