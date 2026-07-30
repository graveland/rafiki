package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDeliverDoesNotBlockForeverOnUnreadSocket verifies that a subscriber
// that stops reading cannot block Deliver forever: the monitor goroutine
// that calls Deliver also drives status transitions and child-exit handling
// for that child, so an unbounded write would stall all bookkeeping.
//
// net.Pipe is fully synchronous and unbuffered (no internal buffering, no
// OS socket buffer), so a single unread Write already blocks immediately —
// there is no "fill the buffer first" step needed the way there would be
// with a real socket. One call is therefore enough to reproduce the
// condition; looping deliverWriteTimeout-bounded calls would only multiply
// the wall-clock cost (each bounded call still takes up to
// deliverWriteTimeout to return) without adding coverage.
//
// This is also the one case exercising the warn side of the classification:
// a write-deadline timeout is the live-subscriber-not-draining case
// deliverWriteTimeout exists to surface, so it must stay at warn.
func TestDeliverDoesNotBlockForeverOnUnreadSocket(t *testing.T) {
	buf := captureSlog(t)

	client, server := net.Pipe() // never read from `client`
	t.Cleanup(func() { client.Close(); server.Close() })

	c := &netConn{conn: server}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Deliver(bytes.Repeat([]byte("x"), 64*1024))
	}()

	select {
	case <-done:
	case <-time.After(deliverWriteTimeout + 5*time.Second):
		t.Fatal("Deliver blocked on an unread socket — monitorChild would stall and the bus would drop terminal frames")
	}

	assertLogLevel(t, buf, "server: deliver frame", slog.LevelWarn)
	if strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("a write-deadline timeout must never be logged at debug — a live subscriber is not draining, frames are being lost; got:\n%s", buf.String())
	}
}

// TestDeliverLogsDebugOnLocalConnAlreadyClosed reproduces the routine race
// between handleConn's deferred conn.Close() (or server shutdown) and
// Broadcast calling Deliver on a connection snapshot taken just before the
// close: SetWriteDeadline and WriteFrame both fail with net.ErrClosed. This
// must log at debug, not warn — the client is already gone and the read
// loop already knows it.
func TestDeliverLogsDebugOnLocalConnAlreadyClosed(t *testing.T) {
	buf := captureSlog(t)

	_, serverSide := newUnixConnPair(t)
	serverSide.Close() // close our own side before Deliver runs

	c := &netConn{conn: serverSide}
	c.Deliver([]byte("hello"))

	out := buf.String()
	if !strings.Contains(out, "server: deliver frame") {
		t.Fatalf("expected a \"server: deliver frame\" log line, got:\n%s", out)
	}
	assertLogLevel(t, buf, "server: deliver frame", slog.LevelDebug)
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a self-closed conn is a routine teardown race, not a warning; got:\n%s", out)
	}
}

// TestDeliverLogsDebugOnPeerGone reproduces an ordinary client disconnect
// (Ctrl-C on `fundi tail`, closing the TUI): the peer's side of the socket
// closes but our side is still open, so WriteFrame fails with syscall.EPIPE.
// This is the exact scenario the brief calls out as log noise today — it
// must land at debug.
func TestDeliverLogsDebugOnPeerGone(t *testing.T) {
	buf := captureSlog(t)

	clientSide, serverSide := newUnixConnPair(t)
	clientSide.Close() // the peer goes away; our side is untouched
	time.Sleep(20 * time.Millisecond)

	c := &netConn{conn: serverSide}
	c.Deliver([]byte("hello"))

	out := buf.String()
	if !strings.Contains(out, "server: deliver frame") {
		t.Fatalf("expected a \"server: deliver frame\" log line, got:\n%s", out)
	}
	assertLogLevel(t, buf, "server: deliver frame", slog.LevelDebug)
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("an ordinary peer disconnect (EPIPE) must not be a warning; got:\n%s", out)
	}
}

// TestIsRoutineConnClose pins the exact classification decision: which
// errors count as a routine connection teardown on this Unix-domain-socket-
// only server, and which stay unclassified (warn). See isRoutineConnClose's
// doc comment for why io.EOF, io.ErrClosedPipe, and syscall.ECONNRESET are
// deliberately excluded.
func TestIsRoutineConnClose(t *testing.T) {
	wrappedClosed := fmt.Errorf("write: %w", net.ErrClosed)
	wrappedEPIPE := &net.OpError{Op: "write", Net: "unix", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"net.ErrClosed", net.ErrClosed, true},
		{"wrapped net.ErrClosed", wrappedClosed, true},
		{"syscall.EPIPE", syscall.EPIPE, true},
		{"OpError-wrapped EPIPE", wrappedEPIPE, true},
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, false},
		{"io.EOF", io.EOF, false},
		{"io.ErrClosedPipe", io.ErrClosedPipe, false},
		{"syscall.ECONNRESET", syscall.ECONNRESET, false},
		{"unrecognized error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRoutineConnClose(tc.err); got != tc.want {
				t.Errorf("isRoutineConnClose(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// captureSlog temporarily replaces the default slog logger with one that
// writes text-format records — including debug — to a buffer, and restores
// the previous default when the test ends.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// assertLogLevel fails the test unless every captured record whose message
// contains msgSubstr was logged at want.
func assertLogLevel(t *testing.T, buf *bytes.Buffer, msgSubstr string, want slog.Level) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	found := false
	for _, line := range lines {
		if line == "" || !strings.Contains(line, `msg="`+msgSubstr+`"`) {
			continue
		}
		found = true
		if !strings.Contains(line, "level="+want.String()) {
			t.Errorf("expected level=%s for line, got:\n%s", want, line)
		}
	}
	if !found {
		t.Fatalf("no log line found with msg=%q in:\n%s", msgSubstr, buf.String())
	}
}

// newUnixConnPair dials a fresh Unix-domain-socket listener and returns the
// connected client and server sides. The listener is closed immediately
// after accepting; both conns are closed on test cleanup (double-Close on an
// already-closed conn is fine).
func newUnixConnPair(t *testing.T) (client, server net.Conn) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "fsk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := dir + "/s.sock"

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- c
	}()

	client, err = net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("Accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept timed out")
	}
	t.Cleanup(func() { server.Close() })

	return client, server
}
