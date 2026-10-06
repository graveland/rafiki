package execpool

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// DialDaemon is the dial step the executor already uses, exported for callers
// that splice the connection rather than speak on it. It must return a usable
// net.Conn.
func TestDialDaemonReturnsAConnection(t *testing.T) {
	ck := assert.NewAborting(t)
	// macOS TempDir paths often exceed the ~104-char unix-socket-path limit.
	dir, err := os.MkdirTemp("/tmp", "ep-")
	ck.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	ck.NoError(err)
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := DialDaemon(ctx, ConnectOptions{SocketPath: sock})
	ck.NoError(err)
	ck.NotNil(conn, "DialDaemon must return a usable connection")
	defer conn.Close()

	// Usable: a byte written on it reaches the accepted peer.
	_, err = conn.Write([]byte("x"))
	ck.NoError(err)

	select {
	case peer := <-accepted:
		defer peer.Close()
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		n, err := peer.Read(buf)
		ck.NoError(err)
		ck.Eq(1, n, "one byte should arrive at the peer")
		ck.Eq("x", string(buf[:n]), "payload")
	case <-time.After(3 * time.Second):
		t.Fatal("the listener never accepted the dial")
	}
}
