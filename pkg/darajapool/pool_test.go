// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// servePoolOnTCP serves pool.UpgradeHandler on a fresh 127.0.0.1 listener and
// returns its address. The server is closed by t.Cleanup; hijacked handlers
// die with the connections the tests close in their own cleanups.
func servePoolOnTCP(t *testing.T, pool *Pool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: pool.UpgradeHandler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// dialUpgrade performs one daraja upgrade against addr with hdr. The dialled
// conn is closed by t.Cleanup.
func dialUpgrade(t *testing.T, addr string, hdr http.Header) (*upgradeconn.Conn, http.Header, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return upgradeconn.Dial(conn, upgradeconn.Daraja, addr, hdr)
}

// schemeHeader builds an Authorization header of the given scheme and secret,
// plus the child id when one is set.
func schemeHeader(scheme upgradeconn.Scheme, secret, childID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", string(scheme)+" "+secret)
	if childID != "" {
		h.Set(upgradeconn.HeaderChildID, childID)
	}
	return h
}

// ticketHeader builds the Authorization a daraja sends on its first dial: the
// one-shot launch ticket, plus the child id it claims.
func ticketHeader(childID, ticket string) http.Header {
	return schemeHeader(upgradeconn.SchemeTicket, ticket, childID)
}

// bearerHeader builds the Authorization a daraja sends on reconnect: the
// credential the daemon minted, plus the child id it is bound to. An empty
// childID omits the header entirely — that is the malformed case.
func bearerHeader(childID, cred string) http.Header {
	return schemeHeader(upgradeconn.SchemeBearer, cred, childID)
}

// waitLive polls until childID is the pool's only live connection.
func waitLive(t *testing.T, pool *Pool, childID string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		live := pool.Live()
		if len(live) == 1 && live[0] == childID {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s never appeared in Live(): %v", childID, live)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// serveTestDaraja plays the daraja side of an upgraded connection: it serves
// HTTP/2 with the stub DarajaService on the conn the test dialled, exactly as
// real daraja does after its upgrade. Without it the pool's relay stream open
// (startIn) never completes, installLive never runs, and the child never
// appears in Live().
func serveTestDaraja(t *testing.T, upConn *upgradeconn.Conn) {
	t.Helper()
	path, handler := darajapbconnect.NewDarajaServiceHandler(stubDaraja{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	h2s := &http2.Server{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h2s.ServeConn(upConn, &http2.ServeConnOpts{Handler: mux})
	}()
	t.Cleanup(func() {
		upConn.Close()
		<-done
	})
}

// TestPoolConnectionStaysUp verifies that after a successful upgrade with a
// ticket, the connection survives past the credential handover. It exercises:
// - the upgrade succeeds and the 101 carries a fresh credential
// - Live() reports the child
// - ClientFor returns a usable client
//
// Previously handleConn blocked on <-lc.done without ever starting the
// Relay stream; idle connections closed immediately. This test proves
// the connection stays up long enough for these checks.
func TestPoolConnectionStaysUp(t *testing.T) {
	reg := NewRegistry()
	tpk, err := reg.MintTicket("c1")
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	upConn, resp, err := dialUpgrade(t, addr, ticketHeader("c1", tpk))
	if err != nil {
		t.Fatalf("upgrade dial: %v", err)
	}
	if cred := resp.Get(upgradeconn.HeaderCredential); cred == "" {
		t.Fatal("expected a credential on the 101 response")
	}

	serveTestDaraja(t, upConn)
	waitLive(t, pool, "c1")

	// ClientFor should return a client.
	if _, err := pool.ClientFor("c1"); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
}

// TestTicketAdmitsAndShowsInLive verifies that a daraja connecting with a valid
// ticket gets admitted and appears in Live().
func TestTicketAdmitsAndShowsInLive(t *testing.T) {
	reg := NewRegistry()
	tk, err := reg.MintTicket("c1")
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	upConn, resp, err := dialUpgrade(t, addr, ticketHeader("c1", tk))
	if err != nil {
		t.Fatalf("upgrade dial: %v", err)
	}
	if cred := resp.Get(upgradeconn.HeaderCredential); cred == "" {
		t.Fatal("expected a credential on the 101 response for first dial")
	}

	serveTestDaraja(t, upConn)
	waitLive(t, pool, "c1")
}

// Unknown ticket is refused terminally: 401 on the upgrade request, no 101.
func TestUnknownTicketIsRefusedTerminally(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	_, _, err := dialUpgrade(t, addr, ticketHeader("c999", "bogus-ticket"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("expected *upgradeconn.Refused, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ref.Status)
	}
	if !strings.Contains(ref.Reason, "ticket is unknown") {
		t.Errorf("reason = %q, want it to name the unknown ticket", ref.Reason)
	}
}

// Wrong child credential is refused: a credential bound to c1 must not admit
// a daraja claiming to be c2.
func TestWrongChildCredentialIsRefused(t *testing.T) {
	reg := NewRegistry()
	cred, err := reg.IssueCredential("c1")
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	_, _, err = dialUpgrade(t, addr, bearerHeader("c2", cred))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("expected *upgradeconn.Refused, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ref.Status)
	}
	if !strings.Contains(ref.Reason, "credential does not match this child") {
		t.Errorf("reason = %q, want the child mismatch", ref.Reason)
	}
}

// Rotating the credential on every upgrade is what makes a displaced (older)
// connection unable to reclaim the child: each admission mints a fresh
// credential, so a credential presented twice only works once.
func TestUpgradeRotatedCredentialInvalidatesThePrevious(t *testing.T) {
	reg := NewRegistry()
	tk, err := reg.MintTicket("c1")
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	// Ticket admits and mints cred1.
	upConn1, resp, err := dialUpgrade(t, addr, ticketHeader("c1", tk))
	if err != nil {
		t.Fatalf("ticket upgrade: %v", err)
	}
	cred1 := resp.Get(upgradeconn.HeaderCredential)
	if cred1 == "" {
		t.Fatal("no credential on the ticket 101")
	}
	serveTestDaraja(t, upConn1) // keep conn 1 in place

	// Bearer cred1 admits and mints cred2, invalidating cred1.
	upConn2, resp2, err := dialUpgrade(t, addr, bearerHeader("c1", cred1))
	if err != nil {
		t.Fatalf("bearer upgrade with cred1: %v", err)
	}
	cred2 := resp2.Get(upgradeconn.HeaderCredential)
	if cred2 == "" || cred2 == cred1 {
		t.Fatalf("credential not rotated: cred1=%q cred2=%q", cred1, cred2)
	}
	serveTestDaraja(t, upConn2)

	// Bearer cred1 again: refused, because cred2 replaced it.
	_, _, err = dialUpgrade(t, addr, bearerHeader("c1", cred1))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("expected *upgradeconn.Refused for the spent credential, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ref.Status)
	}
}

// A Bearer credential without its child id cannot be checked — the registry
// keys credentials by child — so the upgrade is refused 401.
func TestUpgradeBearerWithoutChildIdIs401(t *testing.T) {
	reg := NewRegistry()
	cred, err := reg.IssueCredential("c1")
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	_, _, err = dialUpgrade(t, addr, bearerHeader("", cred))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("expected *upgradeconn.Refused, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ref.Status)
	}
}

// Enroll is the executor's enrollment scheme; daraja never enrolls. The
// upgrade must be refused 401 with a reason that names the valid schemes.
func TestUpgradeEnrollSchemeIsRefused(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	_, _, err := dialUpgrade(t, addr, schemeHeader(upgradeconn.SchemeEnroll, "enroll-token", "c1"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("expected *upgradeconn.Refused, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ref.Status)
	}
	if !strings.Contains(ref.Reason, "daraja does not enroll") {
		t.Errorf("reason = %q, want the no-enroll notice", ref.Reason)
	}
}

// Evict removes a connection immediately.
func TestEvictRemovesConnection(t *testing.T) {
	reg := NewRegistry()
	_ = New(reg) // pool created but we test removeLive directly

	_, _ = reg.IssueCredential("c1")

	p := &Pool{reg: reg, conns: make(map[string]*liveConn)}
	lc := &liveConn{}
	p.conns["c1"] = lc

	evicted := p.removeLive("c1", lc)
	if !evicted {
		t.Fatal("removeLive should have succeeded")
	}
	_, ok := p.conns["c1"]
	if ok {
		t.Error("connection still in map after eviction")
	}
}

// The disconnect callback is what sets the unreachable label, so it must fire
// exactly once — and must NOT fire when a newer connection displaced this one,
// or a reconnect would mark the child unreachable moments after it came back.
func TestDisplacedConnectionDoesNotReportDisconnect(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)

	// Directly exercise installLive / removeLive identity semantics
	// (mirroring execpool's TestHandleConnExitDoesNotEvictAReplacement).

	dispConn := &liveConn{done: make(chan struct{})}
	newConn := &liveConn{done: make(chan struct{})}

	// Install first connection directly (bypassing handleConn)
	pool.conns["c1"] = dispConn

	// installLive replaces it with newConn and tears down the old one
	pool.installLive("c1", newConn)

	// Verify newConn is live
	if pool.conns["c1"] != newConn {
		t.Fatal("expected newConn to be live after displacement")
	}

	// The old connection's handleConn now returns. It calls removeLive for itself.
	// Since dispConn != current mapping, this must return false — preventing
	// OnDisconnect from firing on the displaced connection.
	gone := pool.removeLive("c1", dispConn)
	if gone {
		t.Error("removeLive returned true for a displaced connection — OnDisconnect would fire erroneously")
	}

	// The new connection also exits normally. This SHOULD succeed.
	gone = pool.removeLive("c1", newConn)
	if !gone {
		t.Error("removeLive returned false for the actual live connection")
	}
	_, ok := pool.conns["c1"]
	if ok {
		t.Error("entry still present after removing live connection")
	}
}

// ─── Relay holder tests ────────────────────────────────────────────────────────

// TestRelayHolderFanOutLifecycle verifies subscribe/unsubscribe lifecycle
// on a holder with no active stream (nil client). The channel stays open but
// delivers no events until start() is called and the recvLoop begins receiving.
func TestRelayHolderFanOutLifecycle(t *testing.T) {
	holder := newRelayHolder("c1", nil, nil) // nil client — no stream

	subCh, unsub := holder.subscribe()
	defer unsub()

	// No events before shutdown.
	select {
	case ev := <-subCh:
		t.Fatalf("should not have received event: %+v", ev)
	default:
	}

	// Double-unsubscribe is safe.
	unsub()
	unsub()

	// Still nothing.
	select {
	case ev := <-subCh:
		t.Fatalf("after double unsubscribe should get nothing: %+v", ev)
	default:
	}
}

// TestRelayHolderBroadcastToMultipleSubscribers verifies that broadcast sends
// to all subscriber channels simultaneously (with backpressure via drop).
func TestRelayHolderBroadcastToMultipleSubscribers(t *testing.T) {
	holder := newRelayHolder("c1", nil, nil)

	ch1, unsub1 := holder.subscribe()
	ch2, unsub2 := holder.subscribe()
	defer unsub1()
	defer unsub2()

	testResp := &fanEvent{resp: &darajapb.RelayResponse{
		Event: &darajapb.RelayResponse_Stdout{Stdout: []byte("hello")},
	}}
	holder.broadcast(*testResp)

	// Both should receive.
	var got1, got2 *fanEvent
	select {
	case e := <-ch1:
		got1 = e
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber 1 did not receive")
	}
	select {
	case e := <-ch2:
		got2 = e
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber 2 did not receive")
	}
	if got1.Response().GetStdout() == nil || got2.Response().GetStdout() == nil {
		t.Error("both subscribers should have received the stdout response")
	}
}

// TestRelayHolderClosedRejectsNewSubscribers verifies that after shutdown,
// subscribe returns immediately without adding to fanOut.
func TestRelayHolderClosedRejectsNewSubscribers(t *testing.T) {
	holder := newRelayHolder("c1", nil, nil)
	holder.shutdown()

	_, unsub := holder.subscribe()
	// Must be no-op; unsub is safe to call.
	unsub()

	// Verify fanOut is empty.
	holder.mu.Lock()
	n := len(holder.fanOut)
	holder.mu.Unlock()
	if n != 0 {
		t.Errorf("fanOut should be empty after shutdown, got %d entries", n)
	}
}

// TestPoolEvictCleansUpRelayHolder verifies Evict tears down the relay holder too.
func TestPoolEvictCleansUpRelayHolder(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)

	_cred, _ := reg.IssueCredential("c1")
	_ = _cred
	pool.conns["c1"] = &liveConn{done: make(chan struct{})}
	holder := newRelayHolder("c1", nil, nil)
	pool.relayHolders["c1"] = holder

	pool.Evict("c1")
	if _, ok := pool.conns["c1"]; ok {
		t.Error("conns entry not removed by Evict")
	}
	if _, ok := pool.relayHolders["c1"]; ok {
		t.Error("relayHolders entry not removed by Evict")
	}
}
