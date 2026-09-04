// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── Integration tests: hello frame exchange ─────────────────────────────────

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
