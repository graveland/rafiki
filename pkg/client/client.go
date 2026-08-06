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

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
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

// DialURL opens a TLS connection to rawURL and authenticates via ctrl_auth.
// rawURL must be "tls://host:port". token is sent as the first frame.
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

	dialer := &tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	rawConn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("tls dial %s: %w", u.Host, err)
	}

	// Auth handshake: send ctrl_auth. The server reads one frame, validates
	// the token, and either proceeds silently (success) or closes the
	// connection after writing an error frame (failure). On success we
	// build the Client and start the readLoop — the first frame the
	// readLoop sees will be a real event or response, not an auth confirmation.
	authReq := protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: token}
	authB, _ := json.Marshal(authReq)
	if err := protocol.WriteFrame(rawConn, authB); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}

	// Auth succeeded (server didn't close the connection). Build a Client
	// around the TLS conn. If auth actually failed, the server-side close
	// will cause readLoop to fail, and the next Request/Subscribe will
	// surface the error.
	c := &Client{
		conn:    rawConn,
		closeCh: make(chan struct{}),
		subs:    make(map[uint64]chan []byte),
	}
	go c.readLoop()
	return c, nil
}

// parseControlURL validates and parses a control URL. Scheme must be "tls".
func parseControlURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse control-url: %w", err)
	}
	if u.Scheme != "tls" {
		return nil, fmt.Errorf("control-url scheme must be 'tls', got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("control-url missing host")
	}
	return u, nil
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
			if err != io.EOF {
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
