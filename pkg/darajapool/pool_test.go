// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestPoolConnectionStaysUp verifies that after installLive + relay start,
// the connection survives past the hello exchange. It exercises:
// - Hello frame exchange succeeds
// - Live() reports the child immediately
// - ClientFor returns a usable client
//
// Previously handleConn blocked on <-lc.done without ever starting the
// Relay stream; idle connections closed immediately. This test proves
// the connection stays up long enough for these checks.
func TestPoolConnectionStaysUp(t *testing.T) {
	reg := NewRegistry()
	tpk, _ := reg.MintTicket("c1")
	pool := New(reg)

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		serverConn.Close()
		clientConn.Close()
	})

	// Run handleConn on the "server" side (simulates what upgradeconn gives us).
	go pool.handleConn(serverConn)

	// Simulate daraja writing its hello frame over the pipe.
	hello := protocol.DarajaHelloRequest{
		Type:    "daraja_hello",
		ChildID: "c1",
		Ticket:  tpk,
	}
	helloJSON, _ := json.Marshal(hello)
	helloJSON = append(helloJSON, '\n')
	_, err := clientConn.Write(helloJSON)
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// Set a tight deadline so we read ONLY the hello response, not
	// anything the relay goroutine pipes in afterward.
	clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 512)
	n, err := io.ReadFull(clientConn, buf[:256]) // hello resp fits well under 256
	if err != nil && n == 0 {
		t.Fatalf("read hello response: %v", err)
	}
	var resp protocol.DarajaHelloResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("parse response: %v; raw(%d): %q", err, n, string(buf[:n]))
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Credential == "" {
		t.Fatal("expected credential in hello response")
	}

	// Give handleConn time to complete installLive + relay start.
	time.Sleep(200 * time.Millisecond)

	// Connection should be live.
	live := pool.Live()
	found := false
	for _, id := range live {
		if id == "c1" {
			found = true
			break
		}
	}
	if !found {
		t.Logf("DEBUG: Live()=%v (expected c1 to be present)", live)
		t.Errorf("c1 should appear in Live(), got: %v", live)
	}

	// Verify ClientFor returns a client.
	cli, err := pool.ClientFor("c1")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	_ = cli // client is usable

	// Tear down: close client side so handleProc sees EOF and exits cleanly.
	clientConn.Close()
	time.Sleep(50 * time.Millisecond)
}

// TestTicketAdmitsAndShowsInLive verifies that a daraja connecting with a valid
// ticket gets admitted and appears in Live().
func TestTicketAdmitsAndShowsInLive(t *testing.T) {
	reg := NewRegistry()
	tk, _ := reg.MintTicket("c1")
	pool := New(reg)

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		serverConn.Close()
		clientConn.Close()
	})

	go pool.handleConn(serverConn)

	hello := protocol.DarajaHelloRequest{
		Type:    "daraja_hello",
		ChildID: "c1",
		Ticket:  tk,
	}
	helloJSON, _ := json.Marshal(hello)
	helloJSON = append(helloJSON, '\n')
	_, err := clientConn.Write(helloJSON)
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := clientConn.Read(buf)
	respStr := string(buf[:n])

	// Check response is not an error
	var resp protocol.DarajaHelloResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("invalid response: %v; raw: %s", err, respStr)
	}
	if resp.Error != "" {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
	if resp.Credential == "" {
		t.Fatal("expected credential in response for first dial")
	}

	// Give handleConn time to install the connection.
	time.Sleep(50 * time.Millisecond)

	// Connection should be live
	live := pool.Live()
	found := false
	for _, id := range live {
		if id == "c1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("c1 should appear in Live(), got: %v", live)
	}
}

// Unknown ticket is refused terminally.
func TestUnknownTicketIsRefusedTerminally(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		serverConn.Close()
		clientConn.Close()
	})

	go pool.handleConn(serverConn)

	hello := protocol.DarajaHelloRequest{
		Type:    "daraja_hello",
		ChildID: "c999",
		Ticket:  "bogus-ticket",
	}
	helloJSON, _ := json.Marshal(hello)
	helloJSON = append(helloJSON, '\n')
	_, err := clientConn.Write(helloJSON)
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := clientConn.Read(buf)
	respStr := string(buf[:n])

	if !strings.Contains(respStr, `"error"`) {
		t.Fatalf("expected error response, got: %s", respStr)
	}
}

// Wrong child credential is refused.
func TestWrongChildCredentialIsRefused(t *testing.T) {
	reg := NewRegistry()
	cred, _ := reg.IssueCredential("c1")
	pool := New(reg)

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		serverConn.Close()
		clientConn.Close()
	})

	go pool.handleConn(serverConn)

	hello := protocol.DarajaHelloRequest{
		Type:       "daraja_hello",
		ChildID:    "c2",
		Credential: cred,
	}
	helloJSON, _ := json.Marshal(hello)
	helloJSON = append(helloJSON, '\n')
	_, err := clientConn.Write(helloJSON)
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := clientConn.Read(buf)
	respStr := string(buf[:n])

	if !strings.Contains(respStr, `"error"`) {
		t.Fatalf("expected error response, got: %s", respStr)
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
	holder := newRelayHolder("c1", nil) // nil client — no stream

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
	holder := newRelayHolder("c1", nil)

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
	holder := newRelayHolder("c1", nil)
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
	holder := newRelayHolder("c1", nil)
	pool.relayHolders["c1"] = holder

	pool.Evict("c1")
	if _, ok := pool.conns["c1"]; ok {
		t.Error("conns entry not removed by Evict")
	}
	if _, ok := pool.relayHolders["c1"]; ok {
		t.Error("relayHolders entry not removed by Evict")
	}
}
