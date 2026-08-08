// Package server implements a Unix-domain-socket listener with JSONL framing.
//
// Each accepted connection runs in its own goroutine. Frames are read with
// protocol.NewFrameReader (16 MB cap) and dispatched through a FrameHandler.
// Non-empty handler responses are written back as frames.
package control

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// Connection represents the write side of a single client connection. It is
// passed to every FrameHandler call so that handlers can push unsolicited
// frames (e.g. event delivery for subscriptions) to the specific client.
// Interface identity (type + pointer) is used to key per-connection state
// such as subscription registries, so callers must pass the same object
// for the lifetime of a connection.
type Connection interface {
	// Deliver pushes frame to the client, bounded by deliverWriteTimeout.
	// Errors (including timeouts) are logged, not discarded — see
	// netConn.Deliver.
	Deliver(frame []byte)
}

// FrameHandler is called once per inbound JSONL frame. It may return a
// response frame (written back to the client) or nil/empty to send nothing.
type FrameHandler func(conn Connection, frame []byte) []byte

// ConnectionLifecycleHandler extends FrameHandler with a close notification.
// HandleFrame is called for every inbound frame; HandleClose is called once
// when the connection's read loop ends (EOF, error, or server shutdown).
type ConnectionLifecycleHandler interface {
	HandleFrame(conn Connection, frame []byte) []byte
	HandleClose(conn Connection)
}

// FuncHandler wraps a plain FrameHandler function into a
// ConnectionLifecycleHandler with a no-op HandleClose. Convenient for tests
// and simple handlers that do not need connection-close notifications.
type FuncHandler FrameHandler

func (f FuncHandler) HandleFrame(conn Connection, frame []byte) []byte { return f(conn, frame) }
func (f FuncHandler) HandleClose(_ Connection)                         {}

// Server is a running listener. Call Close to stop it.
type Server struct {
	ln      net.Listener
	handler ConnectionLifecycleHandler
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	ctx     context.Context
	conns   map[Connection]struct{}
	connsMu sync.Mutex
}

// Addr returns the listener's network address. For a UDS server this is
// the socket path; for a TCP server it is the bound host:port.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Listen creates a Unix-domain-socket listener at path. If a stale socket
// file exists at path and no process is actively listening on it, the file is
// removed before binding. If a live process is listening, Listen returns an
// error rather than overwriting the socket.
//
// The socket is chmod'd to 0600 after binding.
func Listen(path string, handler ConnectionLifecycleHandler) (*Server, error) {
	// Stale-socket probe: if the path exists, try to dial. If dial fails the
	// old listener is gone — safe to unlink. If dial succeeds, a live process
	// owns the socket and we must not clobber it.
	if _, err := os.Stat(path); err == nil {
		if dialErr := tryDial(path); dialErr != nil {
			_ = os.Remove(path)
		} else {
			return nil, errors.New("socket in use by a live process")
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ln:      ln,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[Connection]struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

// tryDial probes path with a disposable connection. Error means no listener.
func tryDial(path string) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Distinguish "listener closed by Close()" from real errors.
			if s.ctx.Err() != nil {
				return
			}
			slog.Warn("server: accept", "error", err)
			continue
		}
		// Guard against a Close() that ran between Accept returning and wg.Add:
		// if the context is already cancelled, drop this connection and exit.
		if s.ctx.Err() != nil {
			conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn, nil)
		}()
	}
}

// netConn wraps a net.Conn and implements Connection. Every write to conn
// (Deliver, and handleConn's own response write) goes through writeFrame,
// which serializes them on mu — see writeFrame's doc comment for why.
type netConn struct {
	conn net.Conn
	mu   sync.Mutex
}

// deliverWriteTimeout bounds a single frame write. A subscriber that has
// stopped reading (a suspended terminal, `fundi tail` into a pager) would
// otherwise block this write forever — and Deliver runs on monitorChild's
// goroutine, which also drives status transitions, rename detection and
// child-exit handling. Blocking it stalls the child's whole bookkeeping and
// fills the bus buffer until Publish starts dropping terminal frames
// (agent_settled, message_end), which hangs an attached TUI permanently.
const deliverWriteTimeout = 5 * time.Second

// isRoutineConnClose reports whether err is one of the errors a normal,
// unremarkable connection teardown produces on this server's Unix-domain-
// socket transport: the local conn already closed (net.ErrClosed — e.g.
// handleConn's deferred Close racing Broadcast's outside-lock Deliver call,
// or server shutdown), or the peer already gone (syscall.EPIPE — e.g. a
// client process exiting or disconnecting). Both are things the read loop
// will notice on its own next read; neither implies lost frames the way a
// write-deadline timeout does.
//
// io.EOF is not included: net.Conn.Write never returns it (EOF is a
// read-exhaustion sentinel). io.ErrClosedPipe is not included either: this
// server only ever constructs netConn around a real net.UnixConn from
// Listen's Accept loop, never a net.Pipe, so it would indicate an unexpected
// caller rather than a routine close. syscall.ECONNRESET is not included:
// Unix-domain sockets have no RST/reset semantics, so it does not occur on
// this transport even with unread data buffered at close.
func isRoutineConnClose(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE)
}

// logDeliverErr logs a Deliver-path error at debug if it is a routine
// connection close (see isRoutineConnClose) and at warn otherwise. An
// unrecognized error stays at warn: silently downgrading something we can't
// classify would recreate the original silent-discard problem in a subtler
// form.
func logDeliverErr(msg string, err error, args ...any) {
	args = append([]any{"error", err}, args...)
	if isRoutineConnClose(err) {
		slog.Debug("server: "+msg, args...)
		return
	}
	slog.Warn("server: "+msg, args...)
}

// writeFrame writes one frame to the connection under c.mu, which serializes
// it against every other writer of this connection: Deliver (called from
// DeliverToChild/DeliverToGlobal/DeliverToMatching and Server.Broadcast, each
// on its own goroutine) and handleConn's own response write. This buys two
// things at once:
//
//  1. No interleaving. protocol.WriteFrame issues two writes (payload, then
//     '\n'); holding mu across both means a second goroutine's frame can
//     never be spliced in between them.
//  2. No deadline theft. The deadline is scoped to exactly this write: set
//     immediately before it, cleared immediately after (success, failure, or
//     timeout alike), before mu is released. A goroutine already blocked
//     inside a write is holding mu for the whole blocked duration, so a peer
//     calling writeFrame is blocked on mu, not on the socket — it cannot
//     reach SetWriteDeadline and reset the in-flight write's clock the way
//     an unsynchronized peer could. And because the deadline never outlives
//     its own write, a later write (e.g. handleConn's response, long after a
//     prior Deliver) never inherits a stale, already-expired deadline.
func (c *netConn) writeFrame(frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(deliverWriteTimeout)); err != nil {
		logDeliverErr("set write deadline", err)
		// Fall through: a write without a deadline is still better than no frame.
	}
	err := protocol.WriteFrame(c.conn, frame)
	// Clear the deadline unconditionally (write succeeded, failed, or timed
	// out) so it cannot affect whatever write comes next on this connection.
	if clearErr := c.conn.SetWriteDeadline(time.Time{}); clearErr != nil && err == nil {
		err = clearErr
	}
	return err
}

func (c *netConn) Deliver(frame []byte) {
	if err := c.writeFrame(frame); err != nil {
		// A closed/closing connection is routine — the client went away or
		// the conn already tore down, and the read loop will notice on its
		// next read; logDeliverErr logs that at debug. A write-deadline
		// timeout means a live subscriber is not draining, which is exactly
		// what deliverWriteTimeout exists to catch, so it (and anything else
		// unrecognized) stays at warn — silence would make the resulting
		// frame loss unattributable.
		logDeliverErr("deliver frame", err, "bytes", len(frame))
	}
}

// handleConn drives the frame-read loop for one connection. r, if non-nil,
// is a FrameReader already positioned past a prior auth handshake read (see
// the comment above its use below); pass nil for connections with no
// handshake (the plain UDS listener).
func (s *Server) handleConn(conn net.Conn, r *protocol.FrameReader) {
	defer conn.Close()

	// Close this connection when the server shuts down so ReadFrame unblocks.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go func() {
		select {
		case <-s.ctx.Done():
			conn.Close()
		case <-shutdownDone:
		}
	}()

	nc := &netConn{conn: conn}

	// Register this connection in the broadcast registry.
	s.connsMu.Lock()
	s.conns[nc] = struct{}{}
	s.connsMu.Unlock()
	defer func() {
		s.connsMu.Lock()
		delete(s.conns, nc)
		s.connsMu.Unlock()
	}()

	defer s.handler.HandleClose(nc)
	// r is non-nil when a preceding auth handshake (TCP/TLS path) already
	// constructed a FrameReader and read the auth frame off conn: that
	// bufio.Reader may have buffered bytes past the auth frame's trailing
	// newline (a client's first real request landing in the same TCP
	// segment as ctrl_auth is common — client.DialURL writes auth and
	// returns immediately). Reusing it, instead of wrapping conn in a
	// second FrameReader here, is what keeps those buffered bytes from
	// being silently dropped. The plain UDS path (Listen/acceptLoop) has
	// no handshake, so r is nil and gets a fresh FrameReader as before.
	if r == nil {
		r = protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	}
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if err != io.EOF && s.ctx.Err() == nil {
				slog.Warn("server: read frame", "remote", conn.RemoteAddr(), "error", err)
			}
			return
		}
		resp := s.handler.HandleFrame(nc, frame)
		if len(resp) > 0 {
			// Route through nc.writeFrame (not a bare protocol.WriteFrame on
			// conn) so this response write shares the same per-connection
			// mutex and own-deadline-per-write discipline as Deliver: it
			// cannot interleave with a concurrent subscription frame, and it
			// gets its own fresh deadline rather than firing against one a
			// prior Deliver left behind.
			if err := nc.writeFrame(resp); err != nil {
				if s.ctx.Err() == nil {
					slog.Warn("server: write frame", "remote", conn.RemoteAddr(), "error", err)
				}
				return
			}
		}
	}
}

// Broadcast delivers frame to every currently-active connection.  Best-effort:
// delivery errors are ignored (handled by Connection.Deliver's existing
// semantics).  Returns the number of connections the frame was attempted on.
//
// The registry lock is held only while building the snapshot; Deliver is called
// outside the lock because it may block briefly on slow clients.
func (s *Server) Broadcast(frame []byte) int {
	s.connsMu.Lock()
	conns := make([]Connection, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.connsMu.Unlock()
	for _, c := range conns {
		c.Deliver(frame)
	}
	return len(conns)
}

// Close shuts down the server: it cancels the internal context (which closes
// any in-progress per-connection reads), closes the listener (which unblocks
// the accept loop), and waits for all connection goroutines to finish.
func (s *Server) Close() error {
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

// ─── TCP/TLS listener ──────────────────────────────────────────────────────────

// ListenTCP starts a TLS-wrapped TCP listener that enforces ctrl_auth as the
// mandatory first frame. tlsConfig.Certificates MUST be set before calling —
// TLS is mandatory, no plaintext ever. The handler receives only
// successfully-authed connections.
func ListenTCP(addr string, wantToken string, tlsConfig *tls.Config, handler ConnectionLifecycleHandler) (*Server, error) {
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ln:      ln,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[Connection]struct{}),
	}
	go s.acceptTCPLoop(wantToken)
	return s, nil
}

func (s *Server) acceptTCPLoop(wantToken string) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			slog.Warn("server: tcp accept", "error", err)
			continue
		}
		if s.ctx.Err() != nil {
			conn.Close()
			return
		}

		// Auth handshake runs inside the per-connection goroutine, not
		// here: it used to run inline in this loop, which meant a client
		// that completed the TLS handshake and then sent nothing wedged
		// the accept loop forever — no further connection, from any
		// client, would ever be accepted. authHandshakeTimeout bounds the
		// read so a stalled client only ever costs one goroutine, not the
		// whole control plane.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			r, ok := s.authHandshake(conn, wantToken)
			if !ok {
				conn.Close()
				return
			}
			s.handleConn(conn, r)
		}()
	}
}

// authHandshakeTimeout bounds how long a TCP client has to send a valid
// ctrl_auth frame after TLS establishes. Set as a read deadline for the
// duration of the handshake only and cleared before control passes to
// handleConn, so it never affects the normal request path's read timing.
const authHandshakeTimeout = 10 * time.Second

// authHandshake reads one frame from conn and validates it as a ctrl_auth
// with a matching token. On success it returns the FrameReader used to read
// that frame (which may already hold buffered bytes from a request the
// client pipelined right after auth — see handleConn) and true. On failure
// it writes the appropriate error frame and returns (nil, false).
func (s *Server) authHandshake(conn net.Conn, wantToken string) (*protocol.FrameReader, bool) {
	remote := conn.RemoteAddr()

	if err := conn.SetReadDeadline(time.Now().Add(authHandshakeTimeout)); err != nil {
		slog.Warn("server: auth: set read deadline", "remote", remote, "error", err)
		s.writeAuthError(conn, protocol.ErrAuthRequired, "ctrl_auth required as first frame on TCP connections")
		return nil, false
	}

	r := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := r.ReadFrame()
	if err != nil {
		slog.Warn("server: auth read first frame", "remote", remote, "error", err)
		s.writeAuthError(conn, protocol.ErrAuthRequired, "ctrl_auth required as first frame on TCP connections")
		return nil, false
	}

	var req protocol.AuthRequest
	if err := json.Unmarshal(frame, &req); err != nil || req.Type != protocol.TypeCtrlAuth {
		slog.Warn("server: auth: non-ctrl_auth first frame", "remote", remote)
		s.writeAuthError(conn, protocol.ErrAuthRequired, "ctrl_auth required as first frame on TCP connections")
		return nil, false
	}

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(wantToken)) != 1 {
		slog.Warn("server: auth: invalid token", "remote", remote)
		s.writeAuthError(conn, protocol.ErrAuthInvalid, "invalid auth token")
		return nil, false
	}

	// Clear the deadline before handleConn takes over: a stale deadline
	// left in place would leak into the normal request path and cause
	// spurious read timeouts unrelated to authHandshakeTimeout's purpose.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Warn("server: auth: clear read deadline", "remote", remote, "error", err)
		// Fall through: the handshake itself succeeded, and refusing the
		// connection over a failed deadline-clear would be a worse outcome
		// than the (rare) risk of a stale deadline on this conn.
	}

	return r, true
}

// writeAuthError sends an auth-failure ctrl_response frame and closes the
// connection's write side (a TLS conn will then fail on next read).  Best-effort:
// if the write itself fails the connection is already dead and the caller closes it.
func (s *Server) writeAuthError(conn net.Conn, code, msg string) {
	resp := protocol.Response{
		Type:    protocol.TypeCtrlResponse,
		Command: protocol.TypeCtrlAuth,
		ID:      "0",
		Success: false,
		Error:   &protocol.ErrorBody{Code: code, Message: msg},
	}
	if b, err := json.Marshal(resp); err == nil {
		// Best-effort write — if it fails the caller closes conn anyway.
		_ = protocol.WriteFrame(conn, b)
	}
}
