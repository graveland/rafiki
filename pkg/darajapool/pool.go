// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"golang.org/x/net/http2"
)

// ErrDarajaLost is returned by ClientFor when no daraja is connected for childID.
var ErrDarajaLost = errors.New("darajapool: daraja connection not found")

// ─── live connection ────────────────────────────────────────────────────────

// liveConn is a single active daraja reverse-dialled connection.
type liveConn struct {
	childID string
	httpCli *http.Client                        // inverted h2 client for speaking to daraja
	daraja  darajapbconnect.DarajaServiceClient // built ONCE from httpCli; see ClientFor

	done   chan struct{} // closed when the connection ends
	closed sync.Once     // ensures done closes only once
}

// shutdown closes done if it isn't already, making teardown idempotent.
func (lc *liveConn) shutdown() {
	lc.closed.Do(func() { close(lc.done) })
}

// ─── Pool ───────────────────────────────────────────────────────────────────

// Pool accepts /daraja/connect upgrades, authenticates them on the upgrade
// request itself against the Registry, and holds childID → live daraja
// connections.
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

	// replay is the belt of events broadcast while no subscriber was attached
	// (see relay.go's belt doc comment). Guarded by mu; lock order is always
	// holder.mu → mu, never the reverse (RelayFor's stale-holder stop runs
	// outside the lock for exactly this reason).
	replay map[string][]fanEvent

	onConnectMu    sync.Mutex
	onConnect      map[uint64]func(childID string)
	nextOnConnect  uint64
	onDisconnectMu sync.Mutex
	onDisconnect   []func(childID string)
	onLostMu       sync.Mutex
	onLost         []func(childID string)
}

// New creates a Pool backed by the given Registry.
func New(reg *Registry) *Pool {
	return &Pool{
		reg:          reg,
		conns:        make(map[string]*liveConn),
		relayHolders: make(map[string]*relayHolder),
		replay:       make(map[string][]fanEvent),
		onConnect:    make(map[uint64]func(childID string)),
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
	return upgradeconn.Handler(upgradeconn.Daraja, p.authorize,
		func(c *upgradeconn.Conn, childID string) { p.serve(c, childID) })
}

// authorize authenticates an upgrade request BEFORE the hijack, so a refusal
// is an ordinary HTTP response and never touches the connection. A one-shot
// launch ticket (Ticket) admits the first daraja for a child; a reconnect
// credential presented with its child id (Bearer) admits every later one.
// Either way a fresh credential is minted and returned on the 101, so the
// newest connection is the only one that can come back.
func (p *Pool) authorize(r *http.Request) (string, http.Header, error) {
	scheme, secret, ref := upgradeconn.AuthorizationFrom(r)
	if ref != nil {
		return "", nil, ref
	}

	var childID string
	switch scheme {
	case upgradeconn.SchemeTicket:
		// First launch: redeem the one-shot ticket.
		id, ok := p.reg.RedeemTicket(secret)
		if !ok {
			return "", nil, &upgradeconn.Refusal{
				Status: http.StatusUnauthorized,
				Reason: "ticket is unknown, already used, or revoked",
			}
		}
		childID = id

	case upgradeconn.SchemeBearer:
		// Reconnect: the credential must match the child it claims.
		childID = r.Header.Get(upgradeconn.HeaderChildID)
		if childID == "" || !p.reg.CheckCredential(secret, childID) {
			return "", nil, &upgradeconn.Refusal{
				Status: http.StatusUnauthorized,
				Reason: "credential does not match this child",
			}
		}

	case upgradeconn.SchemeEnroll:
		return "", nil, &upgradeconn.Refusal{
			Status: http.StatusUnauthorized,
			Reason: "daraja does not enroll; send a Ticket or Bearer credential",
		}

	default:
		// AuthorizationFrom returns only the schemes above; a new one must
		// be handled here rather than minting a credential for child "".
		return "", nil, fmt.Errorf("unhandled authorization scheme %q", scheme)
	}

	// Issue a fresh credential, invalidating whatever an older connection
	// still holds. Failure to mint one is a daemon fault, not a peer fault:
	// it becomes a 500. A Bearer peer can retry with the credential it still
	// holds (nothing was replaced); a Ticket peer cannot, because its ticket
	// is already redeemed and the retry is a terminal 401.
	cred, err := p.reg.IssueCredential(childID)
	if err != nil {
		return "", nil, fmt.Errorf("issue credential for %s: %w", childID, err)
	}
	return childID, http.Header{upgradeconn.HeaderCredential: {cred}}, nil
}

// ClientFor returns a daraja Connect client for childID, or an error if the
// daraja is not currently connected.
//
// Must return the SAME client value for the life of one connection: RelayFor
// compares client identity with ==, and NewDarajaServiceClient allocates a
// new pointer per call, so minting one here per call would never compare
// equal to the one the holder was built with. lc.daraja is built once, in
// serve.
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
	return lc.daraja, nil
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
	p.DropReplay(childID)
}

// OnConnect registers a callback invoked when a daraja connects, and returns
// an unsubscribe function that removes it.
//
// WireDaraja's two callbacks are permanent (the daemon's whole lifetime) and
// may ignore the return value. Launch's registration is the reason this
// exists: it needs the callback for exactly one childID, for exactly as long
// as one Launch call is in flight, and every claude spawn registers one — a
// permanent, unremovable list would grow by one per spawn forever and cost
// FireConnect/installLive one dead call per PRIOR spawn on every subsequent
// connect. Launch defers the returned unsubscribe immediately, regardless of
// which of its three outcomes (connected, timeout, ctx done) it hits.
func (p *Pool) OnConnect(fn func(childID string)) (unsubscribe func()) {
	p.onConnectMu.Lock()
	defer p.onConnectMu.Unlock()
	id := p.nextOnConnect
	p.nextOnConnect++
	p.onConnect[id] = fn
	return func() {
		p.onConnectMu.Lock()
		defer p.onConnectMu.Unlock()
		delete(p.onConnect, id)
	}
}

// OnDisconnect registers a callback invoked when a daraja disconnects.
// It fires exactly once per connection lifecycle — NOT when a newer connection
// displaces this one. See TestDisplacedConnectionDoesNotReportDisconnect.
func (p *Pool) OnDisconnect(fn func(childID string)) {
	p.onDisconnectMu.Lock()
	defer p.onDisconnectMu.Unlock()
	p.onDisconnect = append(p.onDisconnect, fn)
}

// OnLost registers a callback invoked when a Runner gives up on a daraja that
// never reconnected within its grace window, just before the Runner reports
// the child exited. It is the one signal that separates "the child's host
// vanished" from an operator kill or a process exit, both of which reach the
// child's exit handler without it.
func (p *Pool) OnLost(fn func(childID string)) {
	p.onLostMu.Lock()
	defer p.onLostMu.Unlock()
	p.onLost = append(p.onLost, fn)
}

// FireLost fires the OnLost callbacks for childID. Exported for tests that
// stand in for a Runner whose grace window ran out.
func (p *Pool) FireLost(childID string) { p.fireLost(childID) }

func (p *Pool) fireLost(childID string) {
	p.onLostMu.Lock()
	fns := slices.Clone(p.onLost)
	p.onLostMu.Unlock()
	for _, fn := range fns {
		fn(childID)
	}
}

// FireConnect fires all registered OnConnect callbacks for childID.
// Only exported for testing — callers outside the package should exercise
// the real connection path (HandleConn / UpgradeHandler) instead.
func (p *Pool) FireConnect(childID string) {
	p.onConnectMu.Lock()
	fns := make([]func(string), 0, len(p.onConnect))
	for _, fn := range p.onConnect {
		fns = append(fns, fn)
	}
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
		displaced.shutdown()
	}

	// Fires on EVERY successful install, first connect included — not just a
	// displacing reconnect. DarajaLaunch waits on this to learn its brand-new
	// daraja arrived, which is never a displacement; scoping the fire to
	// "displaced != nil" left that wait permanently unsatisfied, so it always
	// ran out its 30s and evicted the very connection it was waiting on.
	p.onConnectMu.Lock()
	fns := make([]func(string), 0, len(p.onConnect))
	for _, fn := range p.onConnect {
		fns = append(fns, fn)
	}
	p.onConnectMu.Unlock()
	for _, fn := range fns {
		fn(childID)
	}
}

// removeLive deletes childID's entry only if lc is still the connection mapped
// there, and reports whether it did.
//
// Keying the delete on the childID alone let a stale connection evict its own
// replacement: the old serve exits after a reconnect installed its
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

// serve runs after authorize upgraded the request, with childID already
// resolved and the fresh credential already riding the 101.
func (p *Pool) serve(conn *upgradeconn.Conn, childID string) {
	defer conn.Close()

	// Wrap the upgraded connection into an HTTP/2 client so we can talk to daraja.
	httpClient, err := clientForConn(conn)
	if err != nil {
		slog.Warn("darajapool: http client for conn failed", "childId", childID, "error", err)
		return
	}

	lc := &liveConn{
		childID: childID,
		httpCli: httpClient,
		daraja:  darajapbconnect.NewDarajaServiceClient(httpClient, "http://daraja"),
		done:    make(chan struct{}),
	}

	// Connection death is learned from the relay stream failing (onDone
	// below), backstopped by the http2.Transport's own ReadIdleTimeout/
	// PingTimeout — never from a second goroutine reading conn directly.
	// net/http's connReader stays in "hijacked" state for as long as this
	// handler runs (i.e. the connection's whole life), and a second reader
	// racing the http2.Transport's own internal reader on it panics with
	// "invalid Body.Read call. After hijacked, the original Request must not
	// be used".
	relayCtx, relayCancel := context.WithCancel(context.Background())
	holder := newRelayHolderWithCtx(childID, lc.daraja, relayCtx, relayCancel, func() {
		lc.shutdown()
	}, p)

	// Open the relay stream BEFORE publishing the connection as live.
	// OnConnect (fired by installLive, below) is the signal callers —
	// darajapool.Launch chief among them — use to mean "safe to Send/Watch
	// now". Firing it before the holder's stream had actually opened let a
	// caller race ahead of this open and land on RelayFor's fast path, which
	// finds this holder already present in p.relayHolders (inserted next)
	// and, correctly, does not call start() on it a second time — so the
	// caller got a holder whose stream was still nil. Opening synchronously
	// here, before installLive, closes that window: by the time OnConnect
	// fires the stream is already usable.
	startCtx, startCancel := context.WithTimeout(relayCtx, 5*time.Second)
	err = holder.startIn(startCtx)
	startCancel()
	if err != nil {
		slog.Warn("darajapool: relay start failed", "childId", childID, "error", err)
		relayCancel()
		return
	}

	p.mu.Lock()
	p.relayHolders[childID] = holder
	p.mu.Unlock()

	p.installLive(childID, lc)

	// Block until the connection is done.
	<-lc.done

	gone := p.removeLive(childID, lc)
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
	// but OnDisconnect is NOT called for the displaced peer. The removal must
	// be the one above: a second removeLive here would always find the entry
	// already gone and the callback would never fire.
	if gone {
		p.onDisconnectMu.Lock()
		fns := p.onDisconnect
		p.onDisconnectMu.Unlock()
		for _, fn := range fns {
			fn(childID)
		}
	}
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
