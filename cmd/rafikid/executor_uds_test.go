package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The socket must be 0600 from the moment it exists. A chmod after Listen
// leaves a window in which another local user can connect, and anyone who can
// connect can attempt enrollment.
func TestExecutorUDSIsPrivateFromCreation(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")

	ln, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	if err != nil {
		t.Fatalf("serveExecutorUDS: %v", err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", perm)
	}
}

// A stale socket file from a crashed daemon must not block startup.
func TestExecutorUDSReplacesAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	if err != nil {
		t.Fatalf("serveExecutorUDS refused a stale socket: %v", err)
	}
	defer ln.Close()
}

// A LIVE socket is a different matter: two daemons serving one path means the
// second bind silently wins and the first daemon's executors go nowhere.
func TestExecutorUDSRefusesALiveSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")

	first, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// Prove it is live before asserting the refusal, so a failure here is
	// never ambiguous between "not live" and "refusal missing".
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("first listener is not accepting: %v", err)
	}
	c.Close()

	if _, err := serveExecutorUDS(context.Background(), nil, nil, sock); err == nil {
		t.Error("a second listener bound over a live socket")
	}
}
