// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// readResult is one Read's outcome, handed back from a goroutine.
type readResult struct {
	b   []byte
	err error
}

func readAsync(conn *remoteServeConn, n int) <-chan readResult {
	out := make(chan readResult, 1)
	go func() {
		buf := make([]byte, n)
		k, err := conn.Read(buf)
		out <- readResult{b: buf[:k], err: err}
	}()
	return out
}

func stdoutFrame(b string) *executorpb.RemoteServeResponse {
	return &executorpb.RemoteServeResponse{
		Msg: &executorpb.RemoteServeResponse_Stdout{Stdout: []byte(b)},
	}
}

// A deadline in the past must wake a Read that is blocked waiting for stdout.
// That is exactly what net/http's Hijack does to its background read, and the
// remote sends nothing until it has the 101, so a Read that ignores it never
// returns. Clearing the deadline must leave no data lost.
func TestRemoteServeConnPastDeadlineWakesABlockedRead(t *testing.T) {
	c := assert.NewAborting(t)
	stream := newFakeServeStream()
	conn := newRemoteServeConn(stream, "row-1", "target", "launcher")
	t.Cleanup(func() { _ = conn.Close() })

	pending := readAsync(conn, 16)
	select {
	case r := <-pending:
		c.Require().Fail("Read returned before any stdout or deadline: %q, %v", r.b, r.err)
	case <-time.After(50 * time.Millisecond):
	}

	c.Require().NoError(conn.SetReadDeadline(time.Unix(1, 0)), "set a past deadline")
	select {
	case r := <-pending:
		c.Require().True(errors.Is(r.err, os.ErrDeadlineExceeded),
			"a past deadline must fail the Read with a timeout, got %v", r.err)
		c.Eq(0, len(r.b), "a timed-out Read must consume nothing")
	case <-time.After(5 * time.Second):
		c.Require().Fail("a past read deadline did not wake the blocked Read")
	}

	c.Require().NoError(conn.SetReadDeadline(time.Time{}), "clear the deadline")
	stream.frames <- stdoutFrame("hello")
	select {
	case r := <-readAsync(conn, 16):
		c.Require().NoError(r.err, "read after clearing the deadline")
		c.Eq("hello", string(r.b), "stdout after the deadline was cleared")
	case <-time.After(5 * time.Second):
		c.Require().Fail("Read after clearing the deadline never returned")
	}
}

// A future deadline fires on its own; resetting it before it fires cancels it.
func TestRemoteServeConnFutureDeadlineFiresAndResets(t *testing.T) {
	c := assert.NewAborting(t)
	stream := newFakeServeStream()
	conn := newRemoteServeConn(stream, "row-1", "target", "launcher")
	t.Cleanup(func() { _ = conn.Close() })

	c.Require().NoError(conn.SetReadDeadline(time.Now().Add(20*time.Millisecond)), "set a near deadline")
	select {
	case r := <-readAsync(conn, 16):
		c.Require().True(errors.Is(r.err, os.ErrDeadlineExceeded), "want a timeout, got %v", r.err)
	case <-time.After(5 * time.Second):
		c.Require().Fail("a future read deadline never fired")
	}

	// Re-armed past the point the old timer would have fired, then cleared: the
	// expired state must not leak into the new deadline.
	c.Require().NoError(conn.SetReadDeadline(time.Now().Add(time.Hour)), "re-arm")
	c.Require().NoError(conn.SetReadDeadline(time.Time{}), "clear")
	pending := readAsync(conn, 16)
	select {
	case r := <-pending:
		c.Require().Fail("a cleared deadline still failed the Read: %q, %v", r.b, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	stream.frames <- stdoutFrame("late")
	select {
	case r := <-pending:
		c.Require().NoError(r.err, "read with no deadline")
		c.Eq("late", string(r.b), "stdout with no deadline")
	case <-time.After(5 * time.Second):
		c.Require().Fail("Read with no deadline never returned")
	}
}

// An ACCEPTED tunnelled upgrade must be answered with a 101. A refused one is
// answered without a hijack, so only a valid row-bound ticket exercises the
// path where net/http aborts its background read on this connection.
func TestServeConnAnswersAnAcceptedTunnelUpgrade(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeExecStore()
	row, _, err := store.Create(context.Background(), executors.NewToken{
		Labels:        map[string]string{"machine": "target", "owner": "brent"},
		WorkspaceMode: "pinned",
	})
	c.Require().NoError(err, "create the row")
	pool := execpool.New(store)
	ticket, err := pool.Tickets().Mint(execpool.TicketGrant{RowID: row.ID})
	c.Require().NoError(err, "mint the ticket")

	stream := newFakeServeStream()
	conn := newRemoteServeConn(stream, row.ID, "target", "launcher")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = pool.ServeConn(ctx, conn) }()

	stream.frames <- stdoutFrame("GET " + upgradeconn.PathFor(upgradeconn.Executor) + " HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Authorization: Ticket " + ticket + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: " + string(upgradeconn.Executor) + "\r\n" +
		protocol.EpochHeader + ": " + strconv.Itoa(protocol.Epoch) + "\r\n" +
		"\r\n")

	deadline := time.Now().Add(5 * time.Second)
	for {
		var stdin strings.Builder
		for _, req := range stream.sentRequests() {
			stdin.Write(req.GetStdin())
		}
		if strings.HasPrefix(stdin.String(), "HTTP/1.1 101 ") {
			return
		}
		c.Require().True(time.Now().Before(deadline),
			"no 101 for an accepted upgrade within 5s; the remote was sent %q", stdin.String())
		time.Sleep(10 * time.Millisecond)
	}
}

func stderrFrame(b string) *executorpb.RemoteServeResponse {
	return &executorpb.RemoteServeResponse{
		Msg: &executorpb.RemoteServeResponse_Stderr{Stderr: []byte(b)},
	}
}

func waitExited(t *testing.T, conn *remoteServeConn) {
	t.Helper()
	select {
	case <-conn.exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never ended")
	}
}

// A session that dies without a clean exit must put the remote's own stderr in
// the daemon log at Warn, because Debug is off by default and the install
// ring is read only by a launch that is still waiting.
func TestRemoteServeConnLogsAbnormalEndAtWarn(t *testing.T) {
	cases := []struct {
		name  string
		end   func(s *fakeServeStream)
		wants []string
	}{
		{"nonzero exit", func(s *fakeServeStream) { s.exit(3, "") }, []string{"exited abnormally", "exitCode=3"}},
		{"stream closed without an exit frame", func(s *fakeServeStream) { close(s.frames) }, []string{"without an exit frame"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			logs := captureLogs(t)
			stream := newFakeServeStream()
			conn := newRemoteServeConn(stream, "row-1", "target", "launcher")
			t.Cleanup(func() { _ = conn.Close() })

			stream.frames <- stderrFrame("fatal: out of disk")
			tc.end(stream)
			waitExited(t, conn)
			// Another test's background goroutine logging a Warn into the shared
			// capture, after this connection's own line.
			slog.Warn("execpool: Describe failed on join", "error", "unavailable: io: read")

			// captureLogs swaps the process-wide logger, so a background
			// goroutine of another test can write a Warn into this buffer; only
			// this connection's own lines count.
			var warn string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "installed executor") {
					warn = line
				}
			}
			c.Require().True(warn != "", "no Warn line; logs:\n%s", logs.String())
			c.True(strings.Contains(warn, "fatal: out of disk"), "the Warn line must carry the remote's stderr: %s", warn)
			for _, w := range tc.wants {
				c.True(strings.Contains(warn, w), "Warn line missing %q: %s", w, warn)
			}
		})
	}
}

// hasConnWarn reports whether the captured log holds a Warn from an installed
// executor connection. Warns from other tests' background goroutines also land
// in a captured buffer and say nothing about this connection.
func hasConnWarn(logs string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "installed executor") {
			return true
		}
	}
	return false
}

// A clean exit, and a session this side closed itself, are not faults.
func TestRemoteServeConnCleanOrOwnEndIsNotAWarning(t *testing.T) {
	t.Run("clean exit", func(t *testing.T) {
		logs := captureLogs(t)
		stream := newFakeServeStream()
		conn := newRemoteServeConn(stream, "row-1", "target", "launcher")
		t.Cleanup(func() { _ = conn.Close() })
		stream.exit(0, "")
		waitExited(t, conn)
		assert.NewCollecting(t).False(hasConnWarn(logs.String()), "logs:\n%s", logs.String())
	})
	t.Run("closed by us", func(t *testing.T) {
		logs := captureLogs(t)
		stream := newFakeServeStream()
		conn := newRemoteServeConn(stream, "row-1", "target", "launcher")
		_ = conn.Close()
		close(stream.frames)
		waitExited(t, conn)
		assert.NewCollecting(t).False(hasConnWarn(logs.String()), "logs:\n%s", logs.String())
	})
}
