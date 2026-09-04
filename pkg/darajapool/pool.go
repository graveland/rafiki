// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"golang.org/x/net/http2"
)

// ErrDarajaLost is returned by ClientFor when no daraja is connected for childID.
var ErrDarajaLost = errors.New("darajapool: daraja connection not found")

// ─── live connection ────────────────────────────────────────────────────────

// liveConn is a single active daraja reverse-dialled connection.
type liveConn struct {
	childID string
	httpCli *http.Client // inverted h2 client for speaking to daraja

	done   chan struct{} // closed when the connection ends
	closed sync.Once     // ensures done closes only once
}

// shutdown closes done if it isn't already, making teardown idempotent.
func (lc *liveConn) shutdown() {
	lc.closed.Do(func() { close(lc.done) })
}

// ─── Pool ───────────────────────────────────────────────────────────────────

// Pool accepts /daraja/connect upgrades, authenticates hello frames against
// the Registry, and holds childID → live daraja connections.
//
// Deliberately NOT execpool.Pool. No rows, no health polling, no park windows,
// no workspace provisioning. A daraja is one-to-one with a child the daemon
// already knows and is replaced rather than repaired.
//
// Per-child relay holders (relayHolders map) own ONE Relay stream per child:
// the send direction is serialized through the holder's stdin mutex, and the
// receive loop fans events to Watch subscribers. See relay.go for details.
type Pool struct {
	mu    sync.RWMutex
	reg   *Registry
	conns map[string]*liveConn // childID → live connection

	relayHolders map[string]*relayHolder // childID → relay holder (owned here)

	onConnectMu    sync.Mutex
	onConnect      []func(childID string)
	onDisconnectMu sync.Mutex
	onDisconnect   []func(childID string)
}

// New creates a Pool backed by the given Registry.
func New(reg *Registry) *Pool {
	return &Pool{
		reg:          reg,
		conns:        make(map[string]*liveConn),
		relayHolders: make(map[string]*relayHolder),
		onConnect:    make([]func(childID string), 0),
		onDisconnect: make([]func(childID string), 0),
	}
}

// Reg returns the pool's backing Registry for callers that need to revoke
// credentials (e.g. the controller's Close/Kill paths). The pointer is safe:
// the registry lives exactly as long as the pool and is never replaced.
func (p *Pool) Reg() *Registry { return p.reg }

// UpgradeHandler is the daraja endpoint as an http.Handler, for mounting on a
// mux alongside anything else. The daraja DIALS rafikid and then SERVES HTTP/2;
// rafikid ACCEPTS and is the HTTP client.
func (p *Pool) UpgradeHandler() http.Handler {
	return upgradeconn.Handler(upgradeconn.Daraja, func(c *upgradeconn.Conn) {
		p.handleConn(c)
	})
}

// ClientFor returns a daraja Connect client for childID, or an error if the
// daraja is not currently connected.
func (p *Pool) ClientFor(childID string) (darajapbconnect.DarajaServiceClient, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	lc, ok := p.conns[childID]
	if !ok {
		return nil, fmt.Errorf("daraja %s: %w", childID, ErrDarajaLost)
	}
	if lc.httpCli == nil {
		return nil, fmt.Errorf("daraja %s: unready connection", childID)
	}
	return darajapbconnect.NewDarajaServiceClient(lc.httpCli, "http://daraja"), nil
}

// Live returns all currently connected child IDs.
func (p *Pool) Live() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for childID := range p.conns {
		out = append(out, childID)
	}
	return out
}

// Evict force-closes a daraja's live connection. Idempotent: teardown arrives
// from two directions and closing a channel twice must not panic.
func (p *Pool) Evict(childID string) {
	p.mu.Lock()
	lc, ok := p.conns[childID]
	if ok {
		delete(p.conns, childID)
	}
	holder := p.relayHolders[childID]
	if holder != nil {
		delete(p.relayHolders, childID)
	}
	p.mu.Unlock()

	if ok && lc != nil {
		lc.shutdown()
	}
	if holder != nil {
		holder.stop()
	}
}

// OnConnect registers a callback invoked when a daraja connects.
func (p *Pool) OnConnect(fn func(childID string)) {
	p.onConnectMu.Lock()
	defer p.onConnectMu.Unlock()
	p.onConnect = append(p.onConnect, fn)
}

// OnDisconnect registers a callback invoked when a daraja disconnects.
// It fires exactly once per connection lifecycle — NOT when a newer connection
// displaces this one. See TestDisplacedConnectionDoesNotReportDisconnect.
func (p *Pool) OnDisconnect(fn func(childID string)) {
	p.onDisconnectMu.Lock()
	defer p.onDisconnectMu.Unlock()
	p.onDisconnect = append(p.onDisconnect, fn)
}

// FireConnect fires all registered OnConnect callbacks for childID.
// Only exported for testing — callers outside the package should exercise
// the real connection path (HandleConn / UpgradeHandler) instead.
func (p *Pool) FireConnect(childID string) {
	p.onConnectMu.Lock()
	fns := p.onConnect
	p.onConnectMu.Unlock()
	for _, fn := range fns {
		fn(childID)
	}
}

// FireDisconnect fires all registered OnDisconnect callbacks for childID.
// Only exported for testing — callers outside the package should exercise
// the real connection path (HandleConn / UpgradeHandler) instead.
func (p *Pool) FireDisconnect(childID string) {
	p.onDisconnectMu.Lock()
	fns := p.onDisconnect
	p.onDisconnectMu.Unlock()
	for _, fn := range fns {
		fn(childID)
	}
}

// installLive publishes lc as THE connection for childID, tearing down whatever
// it displaces.
//
// A reconnect installs a new connection under the same id long before the old
// one notices its socket is dead. Both are therefore live at once, and the map
// can only hold one. Displacing the old one is the right call rather than
// refusing the new: a laptop waking up recovers immediately, and making it
// wait out the previous connection's timeout would undo that.
func (p *Pool) installLive(childID string, lc *liveConn) {
	p.mu.Lock()
	displaced := p.conns[childID]
	p.conns[childID] = lc
	p.mu.Unlock()

	if displaced != nil && displaced != lc {
		slog.Info("darajapool: daraja reconnected; tearing down the previous connection",
			"childId", childID)
		// Signal on CONNECT for the NEW connection
		p.onConnectMu.Lock()
		fns := p.onConnect
		p.onConnectMu.Unlock()
		for _, fn := range fns {
			fn(childID)
		}
		displaced.shutdown()
	}
}

// removeLive deletes childID's entry only if lc is still the connection mapped
// there, and reports whether it did.
//
// Keying the delete on the childID alone let a stale connection evict its own
// replacement: the old handleConn exits after a reconnect installed its
// replacement and a remove keyed by ID would wipe out the working one.
func (p *Pool) removeLive(childID string, lc *liveConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.conns[childID]; ok && cur == lc {
		delete(p.conns, childID)
		return true
	}
	return false
}

// ─── handleConn ─────────────────────────────────────────────────────────────

func (p *Pool) handleConn(conn net.Conn) {
	defer conn.Close()

	// Set a read deadline for the hello frame — a silent client must not
	// wedge the accept loop.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	hello, err := readHelloFrame(conn)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		slog.Warn("darajapool: hello read failed", "error", err)
		return
	}

	var childID string
	var credential string

	switch {
	case hello.Ticket != "":
		// First launch: redeem the ticket
		id, ok := p.reg.RedeemTicket(hello.Ticket)
		if !ok {
			writeDarajaHello(conn, protocol.DarajaHelloResponse{
				Type:      "daraja_hello",
				Error:     "ticket is unknown, already used, or revoked",
				Retryable: false,
			})
			return
		}
		childID = id

		// Issue a reconnect credential
		credential, _ = p.reg.IssueCredential(childID)

	case hello.Credential != "":
		// Reconnect: verify the credential
		if !p.reg.CheckCredential(hello.Credential, hello.ChildID) {
			writeDarajaHello(conn, protocol.DarajaHelloResponse{
				Type:      "daraja_hello",
				Error:     "credential does not match this child",
				Retryable: false,
			})
			return
		}
		childID = hello.ChildID
		// Issue a new credential to invalidate older reconnects
		credential, _ = p.reg.IssueCredential(childID)

	default:
		writeDarajaHello(conn, protocol.DarajaHelloResponse{
			Type:      "daraja_hello",
			Error:     "no ticket or credential in hello",
			Retryable: false,
		})
		return
	}

	// Send the hello response with the credential (empty after first dial).
	writeDarajaHello(conn, protocol.DarajaHelloResponse{
		Type:       "daraja_hello",
		Credential: credential,
	})

	// Wrap the upgraded connection into an HTTP/2 client so we can talk to daraja.
	httpClient, err := clientForConn(conn)
	if err != nil {
		slog.Warn("darajapool: http client for conn failed", "childId", childID, "error", err)
		return
	}

	lc := &liveConn{
		childID: childID,
		httpCli: httpClient,
		done:    make(chan struct{}),
	}

	p.installLive(childID, lc)

	// Start the Relay bidi stream so traffic flows on this connection.
	// Without an active stream daraja's HTTP/2 server sees zero requests and
	// closes the connection — exactly the ~1ms-drop symptom. This mirrors
	// execpool's post-install drive (health loop + Describe): we drive
	// activity on the accepted conn so the peer knows we're alive.
	//
	// Two mechanisms keep the connection alive:
	// 1. The relay driver opens a bidi stream; when it exits (stream error),
	//    h.onDone fires lc.shutdown() and unblocks handleProc.
	// 2. A background watcher detects raw-conn closure (EOF) and calls
	//    lc.shutdown() as a backup, covering cases where the stream driver
	//    hasn't launched yet or is stuck in handshake.
	cli := darajapbconnect.NewDarajaServiceClient(lc.httpCli, "http://daraja")
	relayCtx, relayCancel := context.WithCancel(context.Background())
	holder := newRelayHolderWithCtx(childID, cli, relayCtx, relayCancel, func() {
		lc.shutdown()
	})
	p.mu.Lock()
	p.relayHolders[childID] = holder
	p.mu.Unlock()

	// Background watcher: close lc.done when the raw connection drops.
	// Only triggers on permanent errors (EOF); timeout means "still alive, keep watching".
	go func() {
		buf := make([]byte, 1)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, err := conn.Read(buf)
			if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
				// Permanent read error (including EOF) — connection broken.
				lc.shutdown()
				return
			}
			// n > 0 with no error: peer sent data — this is normal h2 traffic,
			// ignore it and keep watching.
			// n == 0 && err == nil (timeout): keep polling.
			_ = n
		}
	}()

	go func() {
		startCtx, startCancel := context.WithTimeout(relayCtx, 5*time.Second)
		defer startCancel()
		if err := holder.startIn(startCtx); err != nil {
			slog.Warn("darajapool: relay start failed",
				"childId", childID, "error", err)
		}
	}()

	// Block until the connection is done.
	<-lc.done

	p.removeLive(childID, lc)
	slog.Info("darajapool: daraja left", "childId", childID)

	// Tear down the relay holder for this connection.
	p.mu.Lock()
	relayHolder := p.relayHolders[childID]
	delete(p.relayHolders, childID)
	p.mu.Unlock()
	if relayHolder != nil {
		relayHolder.stop()
	}

	// Only fire OnDisconnect if WE were the ones who removed it — i.e., this
	// was truly the last (and only) connection for this child. Displacement
	// is handled inside installLive where the old connection's shutdown fires
	// but OnDisconnect is NOT called for the displaced peer.
	gone := p.removeLive(childID, lc)
	if gone {
		p.onDisconnectMu.Lock()
		fns := p.onDisconnect
		p.onDisconnectMu.Unlock()
		for _, fn := range fns {
			fn(childID)
		}
	}
}

// ─── hello frame read/write ─────────────────────────────────────────────────

// readHelloFrame reads the newline-delimited hello frame from conn ONE BYTE
// AT A TIME. An over-buffering reader would consume bytes past the newline
// and leave them unavailable for the HTTP/2 transport that follows.
func readHelloFrame(conn net.Conn) (protocol.DarajaHelloRequest, error) {
	var buf [4096]byte
	n := 0
	for {
		if n >= len(buf) {
			return protocol.DarajaHelloRequest{}, fmt.Errorf("hello frame exceeds %d bytes", len(buf))
		}
		if _, err := io.ReadFull(conn, buf[n:n+1]); err != nil {
			return protocol.DarajaHelloRequest{}, fmt.Errorf("read hello: %w", err)
		}
		if buf[n] == '\n' {
			break
		}
		n++
	}
	var req protocol.DarajaHelloRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return protocol.DarajaHelloRequest{}, fmt.Errorf("parse hello: %w", err)
	}
	return req, nil
}

func writeDarajaHello(conn net.Conn, resp protocol.DarajaHelloResponse) {
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

// ─── inverted HTTP/2 client ─────────────────────────────────────────────────

// clientForConn returns an http.Client that speaks HTTP/2 over exactly this
// already-established connection. Daraja side: the connection was DIALLED BY
// THE EXECUTOR (daraja), so roles invert.
func clientForConn(conn net.Conn) (*http.Client, error) {
	var handedOver atomic.Bool
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
			if handedOver.Swap(true) {
				return nil, errors.New("darajapool: transport requested a second connection; the daraja connection is gone")
			}
			return conn, nil
		},
		ReadIdleTimeout: 15 * time.Second,
		PingTimeout:     10 * time.Second,
	}
	return &http.Client{Transport: tr}, nil
}
