package sandboxrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// shortDir creates a short-named temp dir under /tmp: a default TempDir path
// can exceed the ~104-char unix-socket-path limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "relay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startRelayAt starts Serve on dir in the background and cancels it at cleanup.
// It does not wait for the socket: callers that need it up use waitForSocket or
// waitForEcho, and the stale-socket test must not mistake the old file for the
// new listener.
func startRelayAt(t *testing.T, dir string, dial DialFunc) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, dir, dial) }()
	t.Cleanup(cancel)
	return cancel, errCh
}

// startRelay starts Serve on a fresh dir and waits for the listener to appear.
func startRelay(t *testing.T, dial DialFunc) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	dir := shortDir(t)
	cancel, errCh := startRelayAt(t, dir, dial)
	waitForSocket(t, dir)
	return dir, cancel, errCh
}

// waitForSocket polls until <dir>/daemon.sock exists as a socket file.
func waitForSocket(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, SocketName)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return path
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("relay socket %s never appeared", path)
	return ""
}

// waitForEcho polls until the relay at path accepts a connection and echoes a
// probe, proving a live listener (not merely a leftover socket file).
func waitForEcho(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var d net.Dialer
		conn, err := d.Dial("unix", path)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write([]byte("probe")); err == nil {
				buf := make([]byte, len("probe"))
				if _, err := io.ReadFull(conn, buf); err == nil && string(buf) == "probe" {
					_ = conn.SetDeadline(time.Time{})
					return conn
				}
			}
			_ = conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("relay at %s never echoed", path)
	return nil
}

// dialRelay connects to the relay's unix socket.
func dialRelay(t *testing.T, path string) net.Conn {
	t.Helper()
	var d net.Dialer
	conn, err := d.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial relay %s: %v", path, err)
	}
	return conn
}

// echoUpstream starts a TCP echo server and returns a DialFunc connecting to
// it: every byte written to a dialled connection is echoed back.
func echoUpstream(t *testing.T) DialFunc {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	addr := ln.Addr().String()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// expectClosed asserts the peer end of conn has been closed: a read returns an
// error that is not a timeout.
func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected the connection to be closed, but Read returned a byte")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("expected the connection to be closed, but Read timed out")
	}
}

// Bytes must flow both ways through the splice.
func TestRelaySplicesBothDirections(t *testing.T) {
	c := assert.NewAborting(t)
	dir, _, _ := startRelay(t, echoUpstream(t))
	conn := dialRelay(t, filepath.Join(dir, SocketName))
	defer conn.Close()

	for _, msg := range []string{"ping", "pong"} {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err := conn.Write([]byte(msg))
		c.NoError(err)
		buf := make([]byte, len(msg))
		_, err = io.ReadFull(conn, buf)
		c.NoError(err, "echo of %q", msg)
		c.Eq(msg, string(buf), "echoed bytes")
	}
}

// Eight concurrent connections each echo independently.
func TestRelayServesConcurrentConnections(t *testing.T) {
	dir, _, _ := startRelay(t, echoUpstream(t))
	path := filepath.Join(dir, SocketName)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var d net.Dialer
			conn, err := d.Dial("unix", path)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			want := fmt.Sprintf("conn-%d-payload", i)
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write([]byte(want)); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, len(want))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- err
				return
			}
			if string(buf) != want {
				errs <- fmt.Errorf("conn %d: got %q want %q", i, buf, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// When the client closes, the upstream connection is closed too.
func TestRelayClosesUpstreamWhenClientCloses(t *testing.T) {
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	defer ln.Close()

	upstreamClosed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				close(upstreamClosed)
				return
			}
		}
	}()

	addr := ln.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	dir, _, _ := startRelay(t, dial)
	conn := dialRelay(t, filepath.Join(dir, SocketName))

	// Prove the splice is live before closing.
	_, err = conn.Write([]byte("x"))
	c.NoError(err)
	_ = conn.Close()

	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the client did not close the upstream connection")
	}
}

// When the upstream closes, the client connection is closed too.
func TestRelayClosesClientWhenUpstreamCloses(t *testing.T) {
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close() // the upstream hangs up immediately
	}()

	addr := ln.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	dir, _, _ := startRelay(t, dial)
	conn := dialRelay(t, filepath.Join(dir, SocketName))
	defer conn.Close()

	expectClosed(t, conn)
}

// A failed dial closes the client and the relay keeps serving the next one.
func TestRelayDialFailureClosesClientAndKeepsServing(t *testing.T) {
	c := assert.NewAborting(t)
	echo := echoUpstream(t)

	var mu sync.Mutex
	var calls int
	dial := func(ctx context.Context) (net.Conn, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, errors.New("daemon unreachable")
		}
		return echo(ctx)
	}

	dir, _, _ := startRelay(t, dial)
	path := filepath.Join(dir, SocketName)

	// The first connection's dial fails: the relay must close the client.
	first := dialRelay(t, path)
	expectClosed(t, first)
	_ = first.Close()

	// The relay keeps serving: a later connection is spliced and echoes.
	second := dialRelay(t, path)
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(3 * time.Second))
	_, err := second.Write([]byte("still-alive"))
	c.NoError(err)
	buf := make([]byte, len("still-alive"))
	_, err = io.ReadFull(second, buf)
	c.NoError(err)
	c.Eq("still-alive", string(buf), "the relay must keep serving after a dial failure")
}

// Cancelling ctx closes the listener and every active connection, and Serve
// returns.
func TestRelayCtxCancelClosesActiveConns(t *testing.T) {
	c := assert.NewAborting(t)
	dir, cancel, errCh := startRelay(t, echoUpstream(t))
	conn := dialRelay(t, filepath.Join(dir, SocketName))
	defer conn.Close()

	// Prove the splice is live before cancelling.
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err := conn.Write([]byte("hi"))
	c.NoError(err)
	buf := make([]byte, 2)
	_, err = io.ReadFull(conn, buf)
	c.NoError(err)

	cancel()

	select {
	case err := <-errCh:
		c.NoError(err, "Serve must return nil when ctx is cancelled")
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after ctx was cancelled")
	}

	expectClosed(t, conn)
}

// A regular file at the socket path is refused and left untouched.
func TestRelayRefusesNonSocketAtPath(t *testing.T) {
	c := assert.NewAborting(t)
	dir := shortDir(t)
	path := filepath.Join(dir, SocketName)
	c.NoError(os.WriteFile(path, []byte("not a socket"), 0o644))

	var dialled atomic.Bool
	err, returned := serveUntilReturn(t, dir, func(ctx context.Context) (net.Conn, error) {
		dialled.Store(true)
		return nil, errors.New("must not dial")
	})
	c.True(returned, "Serve must return promptly for a non-socket path")
	c.Error(err, "a non-socket file at the socket path must be refused")
	c.False(dialled.Load(), "the dialer must not run")

	b, rerr := os.ReadFile(path)
	c.NoError(rerr)
	c.Eq("not a socket", string(b), "the non-socket file must be left untouched")
}

// A stale socket file at the path is replaced.
func TestRelayReplacesStaleSocketFile(t *testing.T) {
	c := assert.NewAborting(t)
	dir := shortDir(t)
	path := filepath.Join(dir, SocketName)

	// Leave a stale socket file behind: bind it, then close without unlinking.
	ln, err := net.Listen("unix", path)
	c.NoError(err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	c.NoError(ln.Close())
	fi, err := os.Lstat(path)
	c.NoError(err)
	c.True(fi.Mode()&os.ModeSocket != 0, "precondition: a stale socket file must exist at the path")

	startRelayAt(t, dir, echoUpstream(t))

	conn := waitForEcho(t, path)
	defer conn.Close()
}

// serveUntilReturn runs Serve in the background and reports its error and
// whether it returned within a short deadline. It cancels the context on
// timeout so a hung Serve does not leak.
func serveUntilReturn(t *testing.T, dir string, dial DialFunc) (error, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, dir, dial) }()
	select {
	case err := <-errCh:
		return err, true
	case <-time.After(3 * time.Second):
		return nil, false
	}
}
