// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// shortSockDir returns a temp dir short enough for a unix socket path
// (~104 bytes on darwin); t.TempDir() nests under the full test name.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uds")
	assert.NewAborting(t).NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestListenUnixReadyPublishesAnAcceptingSocket: when listenUnixReady returns,
// path is a 0600 socket that accepts, the temporary name is gone, and Close
// removes path.
func TestListenUnixReadyPublishesAnAcceptingSocket(t *testing.T) {
	c := assert.NewAborting(t)
	path := filepath.Join(shortSockDir(t), "s.sock")

	ln, err := listenUnixReady(path)
	c.NoError(err, "listenUnixReady")
	fi, err := os.Stat(path)
	c.NoError(err, "stat path")
	c.True(fi.Mode()&os.ModeSocket != 0, "path is not a socket: %v", fi.Mode())
	c.Eq(os.FileMode(0o600), fi.Mode().Perm(), "socket mode")
	_, err = os.Stat(readyTempPath(path))
	c.True(os.IsNotExist(err), "the temporary name must not survive: %v", err)

	conn, err := net.Dial("unix", path)
	c.NoError(err, "dial the published socket")
	_ = conn.Close()

	c.NoError(ln.Close(), "Close")
	_, err = os.Stat(path)
	c.True(os.IsNotExist(err), "Close must remove path: %v", err)
}

// TestReadyTempPathNeverLonger: the temporary name must fit wherever path
// fits — a longer one would push a socket path near the sun_path limit over.
func TestReadyTempPathNeverLonger(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, p := range []string{"/run/user/501/rafiki/controller.sock", "/r/executor.sock", "/d/s", "s", "ab"} {
		tmp := readyTempPath(p)
		c.True(len(tmp) <= len(p), "readyTempPath(%q) = %q is longer", p, tmp)
		c.True(tmp != p, "readyTempPath(%q) must differ from path", p)
		c.Eq(filepath.Dir(p), filepath.Dir(tmp), "readyTempPath(%q) must be a sibling", p)
	}
}

// TestListenUnixReadyReplacesAStaleTemporary: a temporary socket left by a daemon that
// died between listen and rename does not block the next start.
func TestListenUnixReadyReplacesAStaleTemporary(t *testing.T) {
	c := assert.NewAborting(t)
	path := filepath.Join(shortSockDir(t), "s.sock")
	c.NoError(os.WriteFile(readyTempPath(path), nil, 0o600), "seed a stale temporary")

	ln, err := listenUnixReady(path)
	c.NoError(err, "listenUnixReady over a stale .tmp")
	defer ln.Close()
	conn, err := net.Dial("unix", path)
	c.NoError(err, "dial")
	_ = conn.Close()
}
