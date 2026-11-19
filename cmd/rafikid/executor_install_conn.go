// SPDX-License-Identifier: Apache-2.0

package main

// remoteServeConn makes one launcher's RemoteServe stream look like a net.Conn,
// which is what lets a tunnelled executor go through the SAME upgrade handler a
// dialled one does: read the request, answer the 101, then speak the executor
// protocol over these bytes.
//
// It parses nothing. stdout is opaque protocol bytes, stderr is opaque
// diagnostics, and the exit frame only marks the end of the stream and records
// what ssh/the remote said — because the diagnostic a dying remote leaves on
// stderr is exactly what a failed install has to hand its caller.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

const (
	// remoteServeStderrRing is how much stderr is kept for an error message. A
	// chatty remote must not grow the daemon's memory, and the last few KiB is
	// where a fatal line is.
	remoteServeStderrRing = 4 << 10

	// remoteServeChunks is how much stdout may be buffered ahead of a reader.
	// Small on purpose: the reader is an http.Server, and a buffer that grows
	// with a stalled peer is a leak.
	remoteServeChunks = 16
)

// remoteServeChunk is one stdout chunk, or the terminal error.
type remoteServeChunk struct {
	b   []byte
	err error
}

// remoteServeConn is a net.Conn over one RemoteServe stream.
type remoteServeConn struct {
	stream     remoteServeStream
	executorID string
	target     string
	launcher   string

	// chunks carries stdout to Read; closed is closed by Close alone, so a
	// blocked Read and a blocked pump both unblock when the session ends.
	chunks chan remoteServeChunk
	closed chan struct{}
	// readDeadline is what lets net/http's Hijack interrupt a pending Read.
	readDeadline *deadline
	// ended is closed when the pump has finished delivering — the exit frame or
	// the stream ending. It is launchInstalled's "it died early" arm.
	ended    chan struct{}
	closeOne sync.Once

	// queue is the tail of the chunk Read already handed out, and terminal is
	// the error Read has already reached. Only Read touches either: net.Conn
	// reads a connection from one goroutine.
	queue    []byte
	terminal error

	mu     sync.Mutex
	code   int32
	signal string
	tail   string // the exit frame's stderr_tail
	ring   []byte // the last remoteServeStderrRing bytes of stderr we saw
}

func newRemoteServeConn(stream remoteServeStream, executorID, target, launcher string) *remoteServeConn {
	c := &remoteServeConn{
		stream:     stream,
		executorID: executorID,
		target:     target,
		launcher:   launcher,
		chunks:     make(chan remoteServeChunk, remoteServeChunks),
		closed:     make(chan struct{}),
		ended:      make(chan struct{}),

		readDeadline: newDeadline(),
	}
	go c.pump()
	return c
}

// pump reads the stream until it ends, routing stdout, stderr and the exit
// frame. It is the only goroutine that receives from the stream, so frames are
// handled in order.
func (c *remoteServeConn) pump() {
	defer close(c.ended)
	for {
		msg, err := c.stream.Receive()
		if err != nil {
			// Either way the remote never reported how it ended, so its stderr
			// is the only account of why.
			if errors.Is(err, io.EOF) {
				c.logAbnormalEnd("installed executor stream ended without an exit frame")
			} else {
				c.logAbnormalEnd("installed executor stream broke", "error", err)
			}
			// A stream that ends without an exit frame still ends the READ side:
			// the http.Server must see the connection go, not hang on it.
			c.push(remoteServeChunk{err: io.EOF})
			return
		}
		switch m := msg.Msg.(type) {
		case *executorpb.RemoteServeResponse_Started:
			slog.Debug("installed executor session started",
				"executor", c.executorID, "target", c.target, "pid", m.Started.GetPid())
		case *executorpb.RemoteServeResponse_Stdout:
			if len(m.Stdout) > 0 && !c.push(remoteServeChunk{b: m.Stdout}) {
				return
			}
		case *executorpb.RemoteServeResponse_Stderr:
			c.noteStderr(m.Stderr)
		case *executorpb.RemoteServeResponse_Exit:
			c.noteExit(m.Exit)
			if m.Exit.GetCode() != 0 || m.Exit.GetSignal() != "" {
				c.logAbnormalEnd("installed executor exited abnormally",
					"exitCode", m.Exit.GetCode(), "signal", m.Exit.GetSignal())
			}
			c.push(remoteServeChunk{err: io.EOF})
			return
		}
	}
}

// push hands one chunk to Read. false means the connection was closed and the
// pump should stop.
func (c *remoteServeConn) push(ch remoteServeChunk) bool {
	select {
	case c.chunks <- ch:
		return true
	case <-c.closed:
		return false
	}
}

// noteStderr records stderr in the ring and logs it at Debug with the executor
// and target, so a postmortem has the remote's own words.
func (c *remoteServeConn) noteStderr(b []byte) {
	if len(b) == 0 {
		return
	}
	c.mu.Lock()
	c.ring = append(c.ring, b...)
	if len(c.ring) > remoteServeStderrRing {
		c.ring = c.ring[len(c.ring)-remoteServeStderrRing:]
	}
	c.mu.Unlock()
	slog.Debug("installed executor stderr",
		"executor", c.executorID, "target", c.target, "stderr", string(b))
}

// logAbnormalEnd logs a session that ended without a clean exit, with the
// remote's own stderr, at Warn: a default-level daemon log must carry the
// reason an installed executor died. A session this side closed on purpose
// (uninstall, cancelled launch) ends the same way and is not a fault, so it
// stays at Debug.
func (c *remoteServeConn) logAbnormalEnd(msg string, attrs ...any) {
	level := slog.LevelWarn
	select {
	case <-c.closed:
		level = slog.LevelDebug
	default:
	}
	args := append([]any{"executor", c.executorID, "target", c.target, "stderr", c.stderrTail()}, attrs...)
	slog.Log(context.Background(), level, msg, args...)
}

func (c *remoteServeConn) noteExit(exit *executorpb.RemoteServeExit) {
	c.mu.Lock()
	c.code = exit.GetCode()
	c.signal = exit.GetSignal()
	c.tail = strings.TrimSpace(exit.GetStderrTail())
	c.mu.Unlock()
}

// Read returns the remote's stdout. It ends with io.EOF when the remote's
// process exits, and with the stream's error when the relay broke.
func (c *remoteServeConn) Read(p []byte) (int, error) {
	if c.terminal != nil {
		return 0, c.terminal
	}
	if len(c.queue) > 0 {
		n := copy(p, c.queue)
		c.queue = c.queue[n:]
		return n, nil
	}
	expired := c.readDeadline.wait()
	select {
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	default:
	}
	select {
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	case ch := <-c.chunks:
		if ch.err != nil {
			c.terminal = ch.err
			return 0, ch.err
		}
		n := copy(p, ch.b)
		c.queue = ch.b[n:]
		return n, nil
	case <-c.closed:
		return 0, io.ErrClosedPipe
	}
}

// Write sends bytes to the remote's stdin. The install ticket rides this, which
// is why it is never in an argv on either machine.
func (c *remoteServeConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := c.stream.Send(&executorpb.RemoteServeRequest{
		Msg: &executorpb.RemoteServeRequest_Stdin{Stdin: append([]byte(nil), p...)},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close ends the request side, which half-closes the remote process's stdin and
// so ends the session.
//
// Closing the STREAM is the mechanism, not cancelling a context: an idle HTTP/2
// client stream observes a cancelled context only on its next read or write, so
// a session that is merely abandoned can keep a remote executor running.
func (c *remoteServeConn) Close() error {
	c.closeOne.Do(func() {
		close(c.closed)
		if err := c.stream.CloseRequest(); err != nil && !errors.Is(err, io.EOF) {
			slog.Debug("installed executor stream close failed",
				"executor", c.executorID, "target", c.target, "error", err)
		}
	})
	return nil
}

// exited is closed once the pump has stopped: the remote exited, or the stream
// broke. It is a launch's "it never connected" arm, so it never has to wait for
// a deadline to learn the answer.
func (c *remoteServeConn) exited() <-chan struct{} { return c.ended }

// stderrTail is what the remote said, for an error message: the exit frame's own
// tail when it carried one, else the last bytes of the stderr we saw.
func (c *remoteServeConn) stderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tail != "" {
		return c.tail
	}
	if len(c.ring) == 0 {
		return "(no output)"
	}
	return strings.TrimSpace(string(c.ring))
}

// exitCode and exitSignal are recorded for the caller's log, not its answer.
func (c *remoteServeConn) exitCode() (int32, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.signal
}

// LocalAddr and RemoteAddr name both ends for the pool's logs. The remote's
// string is the whole provenance of this connection: which ssh target, through
// which launcher.
func (c *remoteServeConn) LocalAddr() net.Addr { return installAddr("rafikid") }
func (c *remoteServeConn) RemoteAddr() net.Addr {
	return installAddr("ssh:" + c.target + " via " + c.launcher)
}

// SetReadDeadline is honoured because net/http depends on it: Hijack aborts the
// server's background one-byte read by setting a deadline in the past and then
// waits for that Read to return. The remote sends nothing more until it has
// the 101, and the 101 is written only after Hijack returns, so an inert read
// deadline deadlocks every accepted upgrade.
//
// Nothing else sets one on this connection: http.Server clears its header
// deadline before the handler runs, and the upgraded session sets none, so a
// remote executor stays free to idle for hours between calls.
func (c *remoteServeConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return nil
}

// SetDeadline sets only the read deadline; see SetWriteDeadline.
func (c *remoteServeConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

// SetWriteDeadline is inert: Write is one stream Send, which the stream's own
// context bounds, and no caller of this connection relies on interrupting it.
func (c *remoteServeConn) SetWriteDeadline(time.Time) error { return nil }

// deadline is a read deadline a blocked Read can select on: wait returns a
// channel that is closed once the deadline has passed. A zero time means none.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	expire chan struct{}
}

func newDeadline() *deadline { return &deadline{expire: make(chan struct{})} }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		// The previous timer already fired and closed expire; a new deadline
		// needs a fresh, open channel.
		<-d.expire
	}
	d.timer = nil
	select {
	case <-d.expire:
		d.expire = make(chan struct{})
	default:
	}
	if t.IsZero() {
		return
	}
	if dur := time.Until(t); dur > 0 {
		expire := d.expire
		d.timer = time.AfterFunc(dur, func() { close(expire) })
		return
	}
	close(d.expire)
}

func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.expire
}

// installAddr is a net.Addr whose Network is the transport's name and whose
// String is what an operator reads.
type installAddr string

func (a installAddr) Network() string {
	if strings.HasPrefix(string(a), "ssh:") {
		return "ssh"
	}
	return "rafikid"
}

func (a installAddr) String() string { return string(a) }
