// Package client provides a JSONL client for the fundi daemon (which speaks the
// pi-controller protocol).
// It connects over a Unix domain socket and multiplexes request/response
// correlation by id so multiple concurrent callers can share one connection.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// Client is a connected JSONL client to the fundi daemon.
// Safe for concurrent use; the request/response correlator multiplexes
// in-flight requests by id.
type Client struct {
	conn    net.Conn
	encMu   sync.Mutex // serializes writes
	pending sync.Map   // map[string]chan *protocol.Response
	nextID  atomic.Uint64
	closed  atomic.Bool
	closeCh chan struct{}

	// readErr stores the first read-loop error for reporting on later requests.
	readErr atomic.Value

	subMu     sync.Mutex
	subs      map[uint64]chan []byte
	nextSubID uint64
}

// Dial opens a connection to the UDS at path. If path is empty,
// resolves to $RAFIKI_SOCKET or the XDG default (see
// DefaultSocketPath).
func Dial(path string) (*Client, error) {
	if path == "" {
		path = DefaultSocketPath()
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	c := &Client{
		conn:    conn,
		closeCh: make(chan struct{}),
		subs:    make(map[uint64]chan []byte),
	}
	go c.readLoop()
	return c, nil
}

// DefaultSocketPath returns the controller socket location: an explicit
// $RAFIKI_SOCKET wins (the daemon sets it for spawned children), else the
// XDG runtime path. This MUST agree with the daemon's own paths.SocketPath, or
// every client dials a socket nobody is listening on.
func DefaultSocketPath() string {
	if p := paths.Get(paths.Socket); p != "" {
		return p
	}
	return paths.SocketPath()
}

// ─── Remote (TCP/TLS) dialing ──────────────────────────────────────────────────

// IsRemoteURL reports whether raw names a remote rafikid worth dialing for
// the CONTROL plane.
//
// Only https does. An http:// URL is the local loopback face — one hostname
// serving the face, the control plane and the executor link is a TLS-only
// arrangement, so there is no plaintext control listener to dial. An empty
// URL means the local daemon. Both keep control on the UDS.
func IsRemoteURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// upgradeTimeout bounds the HTTP upgrade exchange when the caller's context
// carries no deadline of its own. Deliberately generous — it is a liveness
// backstop against a silent server, not a latency budget.
const upgradeTimeout = 15 * time.Second

// DialURL opens a TLS connection to rawURL and, if token is non-empty,
// authenticates via ctrl_auth. rawURL must be "https://host[:port]".
//
// When token is empty, the ctrl_auth frame is skipped entirely: the caller's
// first Request becomes the first frame the server reads. This is what makes
// bootstrap reachable — a daemon with no users admits a connection only when
// its first frame is NOT ctrl_auth, and dispatches that frame as the request
// (restricted to ctrl_user_create). Sending ctrl_auth with an empty token
// would instead resolve to an unknown identity and be rejected outright.
//
// With no ctrl_auth frame, the server's authHandshakeTimeout (10s) bounds
// dial-completion all the way to the CALLER'S FIRST REQUEST, not to a
// handshake write DialURL itself makes — nothing is sent on this path until
// the caller does. A command that dials and then does other work (prompts,
// reads a file) before its first Request risks tripping that budget for
// reasons this doesn't otherwise explain; today's only no-token caller
// (`rafiki user create`) sends immediately, so it doesn't.
//
// Auth success is implicit — the server keeps the connection open.
// Auth failure is detected when the server closes the connection: the
// subsequent read in readLoop will fail with an auth-related read error.
// System root CAs are used.
//
// DialURL blocks until the TLS dial completes or ctx expires.
func DialURL(ctx context.Context, rawURL, token string) (*Client, error) {
	u, err := parseControlURL(rawURL)
	if err != nil {
		return nil, err
	}

	addr := dialAddr(u)
	dialer := &tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	tlsConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tls dial %s: %w", addr, err)
	}

	// Bound the upgrade exchange. Without this a daemon that completes the TLS
	// handshake and then says nothing — wedged, or hostile — hangs the dial
	// forever: upgradeconn.Dial blocks in ReadResponse, ctx has already done
	// its job on the TCP/TLS dial above, and the server's own handshake
	// timeout is no help when the server is the thing that is stuck. Cleared
	// before the Client takes over so it never leaks into the request path.
	if dl, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(dl)
	} else {
		_ = tlsConn.SetDeadline(time.Now().Add(upgradeTimeout))
	}

	// The control plane is reached at a PATH on a shared TLS listener, upgraded
	// out of HTTP/1.1. One port and one certificate serve it, the executor link
	// and anything added later — and an Upgrade tunnel is something every HTTP
	// proxy already understands, so an ingress can carry it without TLS
	// passthrough.
	upConn, err := upgradeconn.Dial(tlsConn, upgradeconn.Control, u.Host)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("clear upgrade deadline: %w", err)
	}
	// Everything from here reads through upConn: bytes the server pipelines
	// behind its 101 would otherwise be stranded in the upgrade's own buffer.
	return serveConn(upConn, token)
}

// serveConn is DialURL's tail: send (or skip) the auth frame on an
// already-connected conn, then hand it to a Client and start reading.
// Split out from DialURL so it is drivable directly over a net.Pipe in
// tests — DialURL itself only ever trusts system root CAs, so a test
// server's self-signed certificate can never get far enough through TLS
// verification to reach this code at all.
func serveConn(conn net.Conn, token string) (*Client, error) {
	if err := sendAuthFrame(conn, token); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}

	// Auth succeeded (server didn't close the connection), or there was
	// nothing to authenticate (no token). Build a Client around the TLS
	// conn. If auth actually failed, the server-side close will cause
	// readLoop to fail, and the next Request/Subscribe will surface the
	// error.
	c := &Client{
		conn:    conn,
		closeCh: make(chan struct{}),
		subs:    make(map[uint64]chan []byte),
	}
	go c.readLoop()
	return c, nil
}

// sendAuthFrame writes a ctrl_auth frame carrying token, or does nothing at
// all when token is empty.
//
// The empty case is not "auth with a blank credential" — it is "skip the
// handshake": a bootstrap daemon (no users yet) admits a connection only when
// its very first frame is NOT ctrl_auth, and then dispatches that frame as
// the request (restricted to ctrl_user_create). Writing a ctrl_auth frame
// with an empty token would instead resolve to an unknown identity on a
// non-bootstrap daemon, or consume the bootstrap slot on a wrong frame type
// on one — either way the caller's real first request would land as the
// SECOND frame and never be read as the bootstrap command.
func sendAuthFrame(conn net.Conn, token string) error {
	if token == "" {
		return nil
	}
	authReq := protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: token}
	authB, err := json.Marshal(authReq)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(conn, authB)
}

// parseControlURL validates a control URL and returns it. Scheme must be
// "https": the control plane is an HTTP upgrade at /control on the shared TLS
// listener, so https is the honest spelling of what tls:// always meant.
func parseControlURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse rafiki url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("RAFIKI_URL scheme must be 'https' to reach the control plane, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("RAFIKI_URL missing host")
	}
	return u, nil
}

// dialAddr returns the TCP dial target for u: its host:port if a port was
// given, else its host with the https default (443) appended. url.URL leaves
// an unspecified port out of u.Host entirely, and net.Dial requires one —
// without this, any RAFIKI_URL of the documented "https://host" form (no
// explicit :443) fails with "missing port in address" before TLS is even
// attempted.
func dialAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	return net.JoinHostPort(u.Hostname(), "443")
}

// Request sends a typed request and waits for the matching response.
// req must marshal to a JSON object with a "type" field; if the
// request has no ID, one is assigned automatically.
func (c *Client) Request(ctx context.Context, req any) (*protocol.Response, error) {
	if c.closed.Load() {
		return nil, errClosedConn(c.readErr.Load())
	}

	// Marshal req to discover/assign id.
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	id, b, err := c.ensureID(b)
	if err != nil {
		return nil, err
	}

	respCh := make(chan *protocol.Response, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	if err := c.send(b); err != nil {
		return nil, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, errClosedConn(c.readErr.Load())
	}
}

// ensureID inspects the marshaled request, assigns a request ID if
// none was set, and returns the (possibly modified) bytes plus the id.
func (c *Client) ensureID(b []byte) (string, []byte, error) {
	var hdr struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}
	if err := json.Unmarshal(b, &hdr); err != nil {
		return "", nil, fmt.Errorf("inspect: %w", err)
	}
	if hdr.ID != "" {
		return hdr.ID, b, nil
	}
	// Assign an auto-id and re-marshal as a generic map.
	id := fmt.Sprintf("c%d", c.nextID.Add(1))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", nil, err
	}
	m["id"] = json.RawMessage(`"` + id + `"`)
	nb, err := json.Marshal(m)
	if err != nil {
		return "", nil, err
	}
	return id, nb, nil
}

func (c *Client) send(b []byte) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return protocol.WriteFrame(c.conn, b)
}

func (c *Client) readLoop() {
	defer close(c.closeCh)
	r := protocol.NewFrameReader(c.conn, protocol.MaxFrameBytes)
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			// A more specific reason may already be stashed below (an
			// auth-failure response with no waiter to deliver it to) — don't
			// let the connection's mundane teardown error (EOF, or a "write
			// on closed pipe" from the peer having already hung up) clobber
			// it. First stored reason wins.
			if err != io.EOF && c.readErr.Load() == nil {
				c.readErr.Store(err)
			}
			c.closed.Store(true)
			return
		}

		// Parse top-level type to route.
		var hdr struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
		}
		if err := json.Unmarshal(frame, &hdr); err != nil {
			continue // malformed frame; ignore
		}

		switch hdr.Type {
		case protocol.TypeCtrlResponse:
			var resp protocol.Response
			if err := json.Unmarshal(frame, &resp); err != nil {
				continue
			}
			if chAny, ok := c.pending.Load(resp.ID); ok {
				select {
				case chAny.(chan *protocol.Response) <- &resp:
				default:
				}
				break
			}
			// No Request is waiting on this id. The one case that matters is
			// a rejected ctrl_auth: DialURL writes it before any Request has
			// registered a waiter (id "0", never reused by a real request),
			// and a no-token bootstrap-or-refused dial gets the SAME shape
			// back for its very first frame, whatever id the caller gave it.
			// Either way the server closes right after, so without this the
			// only thing callers ever see is errClosedConn(nil) —
			// "client connection closed" — with the real reason (auth_invalid,
			// auth_required, ...) silently dropped here and lost forever.
			if resp.Command == protocol.TypeCtrlAuth && !resp.Success {
				c.readErr.Store(authError(&resp))
			}
		default:
			// Event frames (ctrl_event, ctrl_child_*). Hand off to Subscribe
			// when implemented (Task 3). For now, drop.
			c.dispatchEvent(frame)
		}
	}
}

// Subscribe returns a channel that receives every non-response frame
// the client reads. Multiple Subscribe calls return independent channels.
// The returned cancel func removes this subscriber and closes its channel.
//
// The channel is buffered (256 frames). Events are dropped on a full channel
// so a slow consumer cannot block the read loop.
func (c *Client) Subscribe() (<-chan []byte, func(), error) {
	if c.closed.Load() {
		return nil, nil, errClosedConn(c.readErr.Load())
	}
	ch := make(chan []byte, 256)
	c.subMu.Lock()
	c.nextSubID++
	id := c.nextSubID
	c.subs[id] = ch
	c.subMu.Unlock()

	cancel := func() {
		c.subMu.Lock()
		defer c.subMu.Unlock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(ch)
		}
	}
	return ch, cancel, nil
}

// dispatchEvent fans out a frame to all current subscribers.
// It copies the frame for each subscriber so the reader's buffer can be reused.
func (c *Client) dispatchEvent(frame []byte) {
	c.subMu.Lock()
	chs := make([]chan []byte, 0, len(c.subs))
	for _, ch := range c.subs {
		chs = append(chs, ch)
	}
	c.subMu.Unlock()

	for _, ch := range chs {
		cp := make([]byte, len(frame))
		copy(cp, frame)
		select {
		case ch <- cp:
		default:
			// Subscriber channel full; drop this event rather than stall the read loop.
		}
	}
}

// Close shuts down the connection. Any in-flight Request returns an
// error from the closed-channel arm. Close is idempotent.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.subMu.Lock()
	for id, ch := range c.subs {
		close(ch)
		delete(c.subs, id)
	}
	c.subMu.Unlock()
	return c.conn.Close()
}

func errClosedConn(stored any) error {
	if stored == nil {
		return errors.New("client connection closed")
	}
	return fmt.Errorf("client connection closed: %w", stored.(error))
}

// authError renders a failed ctrl_auth response (auth_invalid, auth_required,
// ...) as a Go error, for readLoop to stash in c.readErr when nobody is
// waiting to receive the response frame itself.
func authError(resp *protocol.Response) error {
	if resp.Error == nil {
		return errors.New("auth: rejected")
	}
	if resp.Error.Message == "" {
		return fmt.Errorf("auth: %s", resp.Error.Code)
	}
	return fmt.Errorf("auth: %s: %s", resp.Error.Code, resp.Error.Message)
}
