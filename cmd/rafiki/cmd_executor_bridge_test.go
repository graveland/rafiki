package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/sandboxrelay"
)

// runBridgeArgv drives the bridge command's RunE exactly as the container's
// argv would, and returns the error it produced.
func runBridgeArgv(t *testing.T, argv ...string) error {
	t.Helper()
	cmd := newExecutorBridgeCmd()
	cmd.SetArgs(argv)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

// A relative --listen would resolve against the container's working directory,
// which the launcher never pinned; the socket then lands somewhere the volume
// does not carry and the sandbox silently has no link. Refuse it by name.
func TestExecutorBridgeRejectsRelativeListen(t *testing.T) {
	c := assert.NewAborting(t)
	err := runBridgeArgv(t, "--listen", "relative/dir", "--dial", "127.0.0.1:1")
	c.Error(err, "a relative --listen must be refused")
	c.StrContains(err.Error(), "must be an absolute path", "the error must name the flag, got: %v", err)
}

// --dial must carry both a host and a port: TCPDial would otherwise dial the
// empty address and every splice would fail inside the container, far from the
// argv that caused it.
func TestExecutorBridgeRejectsBadDial(t *testing.T) {
	for _, dial := range []string{"", "nohostport"} {
		t.Run(dial, func(t *testing.T) {
			c := assert.NewAborting(t)
			err := runBridgeArgv(t, "--listen", t.TempDir(), "--dial", dial)
			c.Error(err, "--dial %q must be refused", dial)
			c.StrContains(err.Error(), "must be host:port", "the error must name the flag, got: %v", err)
		})
	}
}

// The whole point of the verb: bytes written to the volume's unix socket reach
// a fresh TCP connection to the launcher's relay and come back. The relay
// never inspects the bytes, so an echo upstream is the full contract.
func TestExecutorBridgeSplicesUnixToTCP(t *testing.T) {
	c := assert.NewAborting(t)

	// The TCP echo the bridge dials in place of a launcher relay.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()

	// A short-named temp dir: a default TempDir path can exceed the unix
	// socket-path (sun_path) limit.
	dir, err := os.MkdirTemp("", "br")
	c.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- sandboxrelay.Serve(ctx, dir, sandboxrelay.TCPDial(ln.Addr().String())) }()

	socket := filepath.Join(dir, sandboxrelay.SocketName)
	waitForBridgeSocket(t, socket)

	var d net.Dialer
	conn, err := d.Dial("unix", socket)
	c.NoError(err, "dial the bridged unix socket")
	defer conn.Close()

	c.NoError(conn.SetDeadline(time.Now().Add(3*time.Second)), "set deadline")
	_, err = conn.Write([]byte("ping\n"))
	c.NoError(err, "write")
	buf := make([]byte, len("ping\n"))
	_, err = io.ReadFull(conn, buf)
	c.NoError(err, "read the splice")
	c.Eq("ping\n", string(buf), "the bridge must splice the payload through to the TCP relay")

	cancel()
	select {
	case err := <-errCh:
		c.NoError(err, "Serve must return nil when ctx is cancelled")
	case <-time.After(3 * time.Second):
		t.Fatal("the bridge relay did not return after ctx was cancelled")
	}
}

// waitForBridgeSocket polls until the relay has bound <dir>/daemon.sock.
func waitForBridgeSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("bridge socket %s never appeared", path)
}
