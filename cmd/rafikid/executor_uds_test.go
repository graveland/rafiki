package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// The socket must be 0600 from the moment it exists. A chmod after Listen
// leaves a window in which another local user can connect, and anyone who can
// connect can attempt enrollment.
func TestExecutorUDSIsPrivateFromCreation(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")

	ln, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	c.Require().NoError(err, "serveExecutorUDS")
	defer ln.Close()

	fi, err := os.Stat(sock)
	c.Require().NoError(err, "stat")
	c.Eq(0o600, fi.Mode().Perm(), "socket mode")
}

// A stale socket file from a crashed daemon must not block startup.
func TestExecutorUDSReplacesAStaleSocket(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	c.NoError(os.WriteFile(sock, nil, 0o600))

	ln, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	c.NoError(err, "serveExecutorUDS refused a stale socket")
	defer ln.Close()
}

// A LIVE socket is a different matter: two daemons serving one path means the
// second bind silently wins and the first daemon's executors go nowhere.
func TestExecutorUDSRefusesALiveSocket(t *testing.T) {
	ck := assert.NewAborting(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")

	first, err := serveExecutorUDS(context.Background(), nil, nil, sock)
	ck.NoError(err)
	defer first.Close()

	// Prove it is live before asserting the refusal, so a failure here is
	// never ambiguous between "not live" and "refusal missing".
	c, err := net.DialTimeout("unix", sock, time.Second)
	ck.NoError(err, "first listener is not accepting")
	c.Close()

	if _, err := serveExecutorUDS(context.Background(), nil, nil, sock); err == nil {
		t.Error("a second listener bound over a live socket")
	}
}
