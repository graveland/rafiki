package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
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

// --- Tests for the C1/C2 fix (Task H1) ---
//
// The tests above use one net.Pipe and one Deliver call each — structurally
// incapable of observing either C1 (a deadline that outlives its write) or
// C2 (concurrent writers racing the deadline / splicing frames), both of
// which only manifest with a second write and a second goroutine. The tests
// below add that second write and second goroutine, and run under -race.

// TestWriteFrameClearsItsDeadlineForLaterUnmanagedWrites establishes
// invariant 1: a write deadline set for one write must not affect any later
// write on the same connection — even one that does not itself go through
// writeFrame.
//
// This is deliberately NOT "call writeFrame twice": writeFrame always sets
// its own fresh deadline before writing, so two sequential writeFrame calls
// would both succeed whether or not the first one cleared its deadline
// afterward — that shape can't distinguish the fix from the bug. The actual
// C1 defect was handleConn's response write, which set no deadline of its
// own at all and simply fired against whatever the connection's deadline
// happened to be. So this test writes "first" through writeFrame, waits past
// deliverWriteTimeout, then writes "second" with a bare protocol.WriteFrame
// directly on the raw conn — modeling any write that does not manage its own
// deadline, exactly as the pre-fix handleConn did. That only succeeds if
// writeFrame cleared its deadline when "first" returned.
func TestWriteFrameClearsItsDeadlineForLaterUnmanagedWrites(t *testing.T) {
	_, serverSide := newUnixConnPair(t)
	c := &netConn{conn: serverSide}

	if err := c.writeFrame([]byte("first")); err != nil {
		t.Fatalf("first writeFrame: %v", err)
	}

	// If writeFrame's deadline had outlived this call, it would now sit
	// squarely in the past — reproducing "subscribe, receive an event, wait
	// 6s, send a request" from the real repro.
	time.Sleep(deliverWriteTimeout + 500*time.Millisecond)

	if err := protocol.WriteFrame(c.conn, []byte("second")); err != nil {
		t.Fatalf("a write with no deadline of its own failed against a deadline writeFrame should have cleared: %v", err)
	}
}

// TestBlockedWriteTimesOutDespiteConcurrentPeerDelivers establishes
// invariants 2 and 4: a write blocked on a non-draining peer must still time
// out within its own budget, and must log at warn, even while a second
// goroutine is concurrently calling Deliver on the SAME connection.
//
// Reproduced pre-fix (per the brief): a blocked write with a 300ms deadline
// survived more than 3s under a 100ms reset ticker. The mechanism: Deliver
// calls SetWriteDeadline before attempting its write, and SetWriteDeadline
// itself never blocks — so even a peer whose own write then gets stuck
// behind the first (nothing drains this connection) still lands one fresh
// deadline reset before it does. Each peer write here is fired from its own
// short-lived goroutine (mirroring DeliverToChild/DeliverToGlobal/
// DeliverToMatching/Broadcast each running on a different real goroutine) so
// that, pre-fix, ticks keep landing fresh resets indefinitely instead of
// stalling behind one earlier peer's own blocked write.
//
// A real Unix-domain-socket pair is required: net.Pipe has its own internal
// write mutex serializing Write calls (though not SetWriteDeadline), which
// would mask the very race this test targets.
func TestBlockedWriteTimesOutDespiteConcurrentPeerDelivers(t *testing.T) {
	buf := captureSlog(t)

	client, server := newUnixConnPair(t) // never read from `client`
	c := &netConn{conn: server}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.Deliver([]byte("peer"))
				}()
			}
		}
	}()

	t.Cleanup(func() {
		close(stop)
		client.Close()
		server.Close()
		wg.Wait()
	})

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Deliver(bytes.Repeat([]byte("x"), 256*1024))
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed > deliverWriteTimeout+2*time.Second {
			t.Fatalf("blocked write took %v to time out, want close to deliverWriteTimeout (%v) despite concurrent peer Delivers on the same connection", elapsed, deliverWriteTimeout)
		}
	case <-time.After(deliverWriteTimeout + 5*time.Second):
		t.Fatal("blocked write never timed out within deliverWriteTimeout+5s — a concurrent peer Deliver kept extending its deadline, reproducing the reported defect")
	}

	assertLogLevel(t, buf, "server: deliver frame", slog.LevelWarn)
}

// TestConcurrentDeliversNeverInterleaveOnTheWire establishes invariant 3:
// two concurrent frame writes must never interleave — every frame on the
// wire is contiguous and parseable.
//
// protocol.WriteFrame issues two separate writes (payload, then '\n'); with
// no synchronization, another goroutine's whole frame can land in the gap
// between them, splicing frames together the same way JSONL framing can
// only recover from by discarding both. This drives two goroutines writing
// distinct, fixed, easily-distinguished payloads at the same connection and
// parses everything the peer receives with the real protocol.FrameReader —
// exactly what a client does — asserting every parsed frame is byte-for-byte
// one of the two payloads, and that exactly as many frames arrive as were
// written.
//
// A real Unix-domain-socket pair is required for the same reason as above:
// net.Pipe's internal write mutex would serialize the two payload writes
// itself and mask the race.
func TestConcurrentDeliversNeverInterleaveOnTheWire(t *testing.T) {
	client, server := newUnixConnPair(t)
	c := &netConn{conn: server}

	const n = 4000
	frameA := bytes.Repeat([]byte("A"), 200)
	frameB := bytes.Repeat([]byte("B"), 200)

	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		for i := 0; i < n; i++ {
			c.Deliver(frameA)
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; i < n; i++ {
			c.Deliver(frameB)
		}
	}()

	type readResult struct {
		frames [][]byte
		err    error
	}
	resCh := make(chan readResult, 1)
	go func() {
		r := protocol.NewFrameReader(client, protocol.MaxFrameBytes)
		frames := make([][]byte, 0, 2*n)
		for len(frames) < 2*n {
			f, err := r.ReadFrame()
			if err != nil {
				resCh <- readResult{frames, err}
				return
			}
			cp := make([]byte, len(f))
			copy(cp, f)
			frames = append(frames, cp)
		}
		resCh <- readResult{frames, nil}
	}()

	writers.Wait()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("reading frames: %v (got %d of %d before the error)", res.err, len(res.frames), 2*n)
		}
		var gotA, gotB int
		for _, f := range res.frames {
			switch {
			case bytes.Equal(f, frameA):
				gotA++
			case bytes.Equal(f, frameB):
				gotB++
			default:
				t.Fatalf("received a frame matching neither writer's payload — frames spliced on the wire: %q", f)
			}
		}
		if gotA != n || gotB != n {
			t.Fatalf("got %d A-frames and %d B-frames, want %d each — frame count mismatch implies a splice merged or split frames", gotA, gotB, n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out reading all frames — a splice likely swallowed a newline, merging two frames into one and starving the reader of the expected count")
	}
}

// TestHandleConnResponseSurvivesStaleSubscriptionDeadline establishes
// invariant 5, as the exact end-to-end C1 reproduction from the brief: a
// response write from handleConn must not inherit or be broken by an
// earlier subscription (Deliver/Broadcast) write's deadline on the same
// connection.
//
// Sequence, matching the real repro: ctrl_subscribe (modeled here by
// Broadcast, which calls Deliver on this connection) → receive the event →
// wait past deliverWriteTimeout (a normal pause for a user reading) → send a
// request on the SAME socket → the response must still arrive, not
// "i/o timeout" followed by connection close.
func TestHandleConnResponseSurvivesStaleSubscriptionDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fsk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := dir + "/s.sock"

	echo := FuncHandler(func(_ Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	srv, err := Listen(path, echo)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(deliverWriteTimeout + 10*time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	r := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)

	// The subscription event that sets this connection's write deadline.
	// Dial returns as soon as the client side of the handshake completes;
	// the server's acceptLoop registers the connection in a separate
	// goroutine, so poll briefly rather than racing it.
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if got = srv.Broadcast([]byte("event-1")); got == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != 1 {
		t.Fatalf("Broadcast delivered to %d conns, want 1 (connection never registered)", got)
	}
	if _, err := r.ReadFrame(); err != nil {
		t.Fatalf("read broadcast frame: %v", err)
	}

	// The normal pause for a user reading — well past the deadline the
	// broadcast set.
	time.Sleep(deliverWriteTimeout + 500*time.Millisecond)

	// A later request on the SAME socket (attach's prompt/steer/abort path).
	if err := protocol.WriteFrame(conn, []byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("handleConn's response write failed against the stale subscription deadline: %v", err)
	}
	if string(resp) != "echo:ping" {
		t.Fatalf("got %q, want %q", resp, "echo:ping")
	}
}

// TestHandleConnResponseNotCorruptedByConcurrentBroadcast strengthens
// invariant 5 with genuine contention, which
// TestHandleConnResponseSurvivesStaleSubscriptionDeadline does not exercise:
// that test's Broadcast call completes (and its writeFrame clears the
// deadline it set) long before the later request, so it can only prove the
// sequential/quiescent case. It cannot distinguish "handleConn's response
// shares netConn's mutex" from "Deliver alone clears its own deadline in
// time" — both make that particular scenario pass. (Confirmed empirically:
// it still passes with handleConn's response write reverted to a bare,
// unsynchronized protocol.WriteFrame, as long as Deliver's own
// clear-after-write fix is left in place.)
//
// This test instead drives a Broadcast loop on a second goroutine
// CONCURRENTLY with a client hammering requests on the same connection, so
// handleConn's response write and Deliver's subscription write are
// genuinely in flight together, at the same time, on the same net.Conn. If
// handleConn's response write does not share netConn's mutex, the two
// writers' payload+'\n' pairs can interleave on the wire (the same
// splicing mechanism as TestConcurrentDeliversNeverInterleaveOnTheWire, but
// this time with handleConn's real call site as one of the two writers
// instead of two synthetic Deliver calls) — which corrupts frames and/or
// desyncs the expected counts and ordering asserted below.
func TestHandleConnResponseNotCorruptedByConcurrentBroadcast(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fsk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := dir + "/s.sock"

	echo := FuncHandler(func(_ Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	srv, err := Listen(path, echo)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	r := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)

	// Warm-up round trip: guarantees the connection is registered in
	// s.conns before the broadcaster goroutine starts, without racing
	// acceptLoop's registration the way a bare Dial would.
	if err := protocol.WriteFrame(conn, []byte("ping:warmup")); err != nil {
		t.Fatalf("warmup write: %v", err)
	}
	if resp, err := r.ReadFrame(); err != nil || string(resp) != "echo:ping:warmup" {
		t.Fatalf("warmup round trip: resp=%q err=%v", resp, err)
	}

	const n = 4000
	var broadcaster sync.WaitGroup
	broadcaster.Add(1)
	go func() {
		defer broadcaster.Done()
		for i := 0; i < n; i++ {
			srv.Broadcast([]byte(fmt.Sprintf("EVNT:%06d", i)))
		}
	}()

	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < n; i++ {
			if err := protocol.WriteFrame(conn, []byte(fmt.Sprintf("ping:%06d", i))); err != nil {
				return // surfaced via the reader's frame-count mismatch below
			}
		}
	}()

	type readResult struct {
		echoes []string
		events []string
		err    error
	}
	resCh := make(chan readResult, 1)
	go func() {
		echoes := make([]string, 0, n)
		events := make([]string, 0, n)
		for len(echoes) < n || len(events) < n {
			f, err := r.ReadFrame()
			if err != nil {
				resCh <- readResult{echoes, events, err}
				return
			}
			s := string(f)
			switch {
			case strings.HasPrefix(s, "echo:ping:"):
				echoes = append(echoes, s)
			case strings.HasPrefix(s, "EVNT:"):
				events = append(events, s)
			default:
				resCh <- readResult{echoes, events, fmt.Errorf("frame matching neither writer's payload — spliced on the wire: %q", s)}
				return
			}
		}
		resCh <- readResult{echoes, events, nil}
	}()

	writer.Wait()
	broadcaster.Wait()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("reading frames: %v (got %d echoes, %d events before the error)", res.err, len(res.echoes), len(res.events))
		}
		if len(res.echoes) != n {
			t.Fatalf("got %d echo responses, want %d", len(res.echoes), n)
		}
		if len(res.events) != n {
			t.Fatalf("got %d broadcast events, want %d", len(res.events), n)
		}
		for i, e := range res.echoes {
			want := fmt.Sprintf("echo:ping:%06d", i)
			if e != want {
				t.Fatalf("echo response %d out of order or corrupted: got %q, want %q", i, e, want)
			}
		}
		for i, e := range res.events {
			want := fmt.Sprintf("EVNT:%06d", i)
			if e != want {
				t.Fatalf("broadcast event %d out of order or corrupted: got %q, want %q", i, e, want)
			}
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out reading all frames — a splice likely swallowed a newline, merging two frames into one and starving the reader of the expected count")
	}
}

// syncLogBuf is a bytes.Buffer guarded by a mutex. The default logger is
// process-global: while the ticker goroutines below are still Delivering (they
// stop only at cleanup), a Deliver that errors logs into this buffer from ITS
// goroutine, and the test goroutine reads it from assertLogLevel. A bare
// bytes.Buffer is a data race between those two, and -race caught exactly that
// in TestBlockedWriteTimesOutDespiteConcurrentPeerDelivers.
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog temporarily replaces the default slog logger with one that
// writes text-format records — including debug — to a buffer, and restores
// the previous default when the test ends.
func captureSlog(t *testing.T) *syncLogBuf {
	t.Helper()
	buf := &syncLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// assertLogLevel fails the test unless every captured record whose message
// contains msgSubstr was logged at want.
func assertLogLevel(t *testing.T, buf *syncLogBuf, msgSubstr string, want slog.Level) {
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
