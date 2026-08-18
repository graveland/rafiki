// Package server implements a Unix-domain-socket listener with JSONL framing.
//
// Each accepted connection runs in its own goroutine. Frames are read with
// protocol.NewFrameReader (16 MB cap) and dispatched through a FrameHandler.
// Non-empty handler responses are written back as frames.
package control

import (
	"context"
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
	"go.graveland.dev/rafiki/pkg/users"
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

	// Identity is the authenticated caller. The zero value means "not a
	// user": a UDS connection (locally trusted, no handshake) or a
	// bootstrap connection (no users exist yet).
	Identity() users.Identity

	// Restricted reports a bootstrap-admitted connection, which may send
	// only ctrl_user_create. UDS connections are never restricted — the
	// socket is the local trust boundary and always has been.
	Restricted() bool
}

// Authenticator resolves control-plane credentials. It is the users.Store
// interface narrowed to what the handshake needs, so pkg/control does not
// depend on a database package.
type Authenticator interface {
	// Authenticate resolves a ctrl_auth token. users.ErrNotFound means the
	// token is invalid — an answer. Any other error means the check could
	// not be performed and MUST NOT be reported as an auth failure.
	Authenticate(ctx context.Context, token string) (users.Identity, error)

	// CountActive reports how many users exist. Zero puts the listener in
	// bootstrap mode: a connection is admitted without a token and may send
	// only ctrl_user_create.
	CountActive(ctx context.Context) (int, error)
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
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil // an attached server owns no listener; see NewAttached
	}
	return s.ln.Addr()
}

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
			s.handleConn(conn, admission{})
		}()
	}
}

// netConn wraps a net.Conn and implements Connection. Every write to conn
// (Deliver, and handleConn's own response write) goes through writeFrame,
// which serializes them on mu — see writeFrame's doc comment for why.
type netConn struct {
	conn net.Conn
	mu   sync.Mutex

	// identity and restricted are decided once, by the auth handshake,
	// before any frame is dispatched, and never change afterwards — so
	// they need no lock. The zero values are the UDS path's: locally
	// trusted, not a user, not restricted.
	identity   users.Identity
	restricted bool
}

func (c *netConn) Identity() users.Identity { return c.identity }
func (c *netConn) Restricted() bool         { return c.restricted }

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

// admission is what the auth handshake decided about one connection. Its
// zero value is the plain UDS path: no handshake ran, so there is no reader
// to inherit, no user, and no restriction.
type admission struct {
	// reader read the handshake frame and may already hold bytes the client
	// pipelined behind it — handleConn must reuse it, never rebuild it. Nil
	// means no handshake ran.
	reader *protocol.FrameReader

	// identity is the authenticated caller; the zero value means "not a user".
	identity users.Identity

	// restricted marks a bootstrap connection: admitted without a token and
	// permitted only ctrl_user_create (enforced by the dispatcher).
	restricted bool

	// pending is a frame already consumed from reader that is a REQUEST
	// rather than a handshake frame — the bootstrap ctrl_user_create. It is
	// gone from the socket and cannot be read again, so handleConn must
	// dispatch it before entering its read loop.
	pending []byte
}

// handleConn drives the frame-read loop for one connection. Pass the zero
// admission for connections with no handshake (the plain UDS listener).
func (s *Server) handleConn(conn net.Conn, a admission) {
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

	nc := &netConn{conn: conn, identity: a.identity, restricted: a.restricted}

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

	// a.pending is a request frame the handshake already consumed from
	// a.reader (the bootstrap ctrl_user_create). Those bytes are gone from
	// the socket: re-reading conn would block forever, so it is dispatched
	// here, before the loop, or it is lost.
	if len(a.pending) > 0 && !s.serveFrame(nc, a.pending) {
		return
	}

	// a.reader is non-nil when a preceding auth handshake (TCP/TLS path)
	// already constructed a FrameReader and read the first frame off conn:
	// that bufio.Reader may have buffered bytes past the frame's trailing
	// newline (a client's first real request landing in the same TCP
	// segment as ctrl_auth is common — client.DialURL writes auth and
	// returns immediately). Reusing it, instead of wrapping conn in a
	// second FrameReader here, is what keeps those buffered bytes from
	// being silently dropped. The plain UDS path (Listen/acceptLoop) has
	// no handshake, so it is nil and gets a fresh FrameReader as before.
	r := a.reader
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
		if !s.serveFrame(nc, frame) {
			return
		}
	}
}

// serveFrame dispatches one frame and writes any response back. It reports
// whether the connection may continue.
func (s *Server) serveFrame(nc *netConn, frame []byte) bool {
	resp := s.handler.HandleFrame(nc, frame)
	if len(resp) == 0 {
		return true
	}
	// Route through nc.writeFrame (not a bare protocol.WriteFrame on conn)
	// so this response write shares the same per-connection mutex and
	// own-deadline-per-write discipline as Deliver: it cannot interleave
	// with a concurrent subscription frame, and it gets its own fresh
	// deadline rather than firing against one a prior Deliver left behind.
	if err := nc.writeFrame(resp); err != nil {
		if s.ctx.Err() == nil {
			slog.Warn("server: write frame", "remote", nc.conn.RemoteAddr(), "error", err)
		}
		return false
	}
	return true
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
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	s.wg.Wait()
	return err
}

// NewAttached returns a Server with no listener of its own, for connections
// delivered by someone else's accept loop.
//
// That someone else is the shared TLS listener's HTTP mux: the control plane is
// reached at a PATH there, upgraded out of HTTP, and the resulting connection
// handed here. One port and one certificate serve the control plane, the
// executor link and anything added later, and each is routed by path rather
// than by sniffing its first bytes.
func NewAttached(handler ConnectionLifecycleHandler) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[Connection]struct{}),
	}
}

// ServeUpgraded runs the control protocol on a connection someone else
// accepted: the same ctrl_auth handshake and request loop the TCP listener
// runs, minus the accept.
//
// conn MUST be the upgraded connection wrapper, not the raw socket underneath
// it. A client routinely pipelines its first request behind the auth frame, and
// after an HTTP upgrade those bytes sit in the hijack buffer rather than on the
// socket — reading past the wrapper drops them and hangs the client to its
// timeout. The wrapper reads the buffer first, so authHandshake's FrameReader
// sees an unbroken stream.
func (s *Server) ServeUpgraded(conn net.Conn, auth Authenticator) {
	s.wg.Add(1)
	defer s.wg.Done()

	a, ok := s.authHandshake(conn, auth)
	if !ok {
		conn.Close()
		return
	}
	s.handleConn(conn, a)
}

// ─── TCP/TLS listener ──────────────────────────────────────────────────────────

// ListenTCP starts a TLS-wrapped TCP listener that enforces ctrl_auth as the
// mandatory first frame, resolving its token through auth.
// tlsConfig.Certificates MUST be set before calling — TLS is mandatory, no
// plaintext ever. The handler receives only admitted connections: those that
// authenticated, plus — while auth reports zero users — bootstrap connections,
// which arrive with no token and are marked Restricted so the dispatcher
// accepts only ctrl_user_create on them.
func ListenTCP(addr string, auth Authenticator, tlsConfig *tls.Config, handler ConnectionLifecycleHandler) (*Server, error) {
	// Fail here rather than per connection: a nil authenticator is a wiring
	// mistake, and the loud version of it is a listener that never binds.
	if auth == nil {
		return nil, errors.New("control: ListenTCP requires an Authenticator")
	}
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
	go s.acceptTCPLoop(auth)
	return s, nil
}

func (s *Server) acceptTCPLoop(auth Authenticator) {
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
			a, ok := s.authHandshake(conn, auth)
			if !ok {
				conn.Close()
				return
			}
			s.handleConn(conn, a)
		}()
	}
}

// authHandshakeTimeout bounds how long a TCP client has to send a valid
// ctrl_auth frame after TLS establishes. Set as a read deadline for the
// duration of the handshake only and cleared before control passes to
// handleConn, so it never affects the normal request path's read timing.
const authHandshakeTimeout = 10 * time.Second

// authHandshake reads the first frame and decides how the connection may
// proceed: as an authenticated user, as a bootstrap connection (no users
// exist yet, and the frame it just read is the ctrl_user_create that creates
// the first one), or not at all. On failure it writes the appropriate error
// frame and returns ok=false; the caller closes the connection.
func (s *Server) authHandshake(conn net.Conn, auth Authenticator) (admission, bool) {
	remote := conn.RemoteAddr()
	refuse := func(code, msg string) (admission, bool) {
		s.writeAuthError(conn, code, msg)
		return admission{}, false
	}
	const authRequired = "ctrl_auth required as first frame on TCP connections"

	// ListenTCP rejects a nil authenticator up front, but ServeUpgraded has
	// no error to return, and this runs on a per-connection goroutine: a nil
	// deref here would take the whole daemon down over one connection.
	// Refusing everything is the safe reading of "no way to check".
	if auth == nil {
		slog.Error("server: auth: no authenticator configured", "remote", remote)
		return refuse(protocol.ErrInternal, "identity store unavailable")
	}

	if err := conn.SetReadDeadline(time.Now().Add(authHandshakeTimeout)); err != nil {
		slog.Warn("server: auth: set read deadline", "remote", remote, "error", err)
		return refuse(protocol.ErrAuthRequired, authRequired)
	}

	r := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := r.ReadFrame()
	if err != nil {
		slog.Warn("server: auth read first frame", "remote", remote, "error", err)
		return refuse(protocol.ErrAuthRequired, authRequired)
	}

	ctx, cancel := context.WithTimeout(s.ctx, authHandshakeTimeout)
	defer cancel()

	var req protocol.AuthRequest
	if err := json.Unmarshal(frame, &req); err != nil || req.Type != protocol.TypeCtrlAuth {
		// Not a ctrl_auth frame. This is legal in exactly one situation: no
		// user exists yet, and the frame is the ctrl_user_create that
		// creates the first one. The dispatcher enforces the command
		// restriction; the handshake only decides admission.
		n, cerr := auth.CountActive(ctx)
		if cerr != nil {
			// "I could not check" — never reported as an auth answer, and
			// never with the store's error text.
			slog.Error("server: auth: cannot determine bootstrap state", "remote", remote, "error", cerr)
			return refuse(protocol.ErrInternal, "identity store unavailable")
		}
		if n > 0 {
			slog.Warn("server: auth: non-ctrl_auth first frame", "remote", remote)
			return refuse(protocol.ErrAuthRequired, authRequired)
		}
		slog.Warn("server: BOOTSTRAP connection admitted without a token — no users exist; "+
			"the first ctrl_user_create claims this daemon", "remote", remote)
		s.clearDeadline(conn, remote)
		// The frame just read is the client's first REQUEST, not a
		// handshake frame: hand it back so handleConn dispatches it. It has
		// already been consumed from the socket and cannot be recovered any
		// other way.
		return admission{reader: r, restricted: true, pending: frame}, true
	}

	id, err := auth.Authenticate(ctx, req.Token)
	if errors.Is(err, users.ErrNotFound) {
		slog.Warn("server: auth: invalid token", "remote", remote)
		return refuse(protocol.ErrAuthInvalid, "invalid auth token")
	}
	if err != nil {
		// "I could not check" — never reported as an invalid credential,
		// and never with the store's error text: this peer has not proved
		// who it is and a pgx error carries the DSN.
		slog.Error("server: auth: identity store unavailable", "remote", remote, "error", err)
		return refuse(protocol.ErrInternal, "identity store unavailable")
	}

	s.clearDeadline(conn, remote)
	return admission{reader: r, identity: id}, true
}

// clearDeadline drops the handshake read deadline before handleConn takes
// over: a stale deadline left in place would leak into the normal request
// path and cause spurious read timeouts unrelated to authHandshakeTimeout's
// purpose. A failure here is logged and ignored — refusing the connection
// over a failed deadline-clear would be a worse outcome than the (rare) risk
// of a stale deadline.
func (s *Server) clearDeadline(conn net.Conn, remote net.Addr) {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Warn("server: auth: clear read deadline", "remote", remote, "error", err)
	}
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
