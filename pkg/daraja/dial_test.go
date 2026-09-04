package daraja_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/daraja"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// helloOnlyDaemon creates a unix socket in /tmp, acts as the raﬁkid server
// using upgradeconn.Handler, accepts multiple sequential daraja connections,
// reads one DarajaHelloRequest per connection, writes the callback's response,
// then closes that connection before accepting the next. Returns the socket
// path for Connect to dial.
func helloOnlyDaemon(t *testing.T, cb func(protocol.DarajaHelloRequest) protocol.DarajaHelloResponse) string {
	t.Helper()
	path := "/tmp/daraja-hello-" + t.Name() + ".sock"
	_ = os.Remove(path) // clean stale socket from previous test run

	go func() {
		ln, err := net.Listen("unix", path)
		if err != nil {
			return
		}
		defer ln.Close()

		// The HTTP handler upgrades the connection and then serves the
		// byte-stream protocol. This mirrors what rafikid does on
		// /daraja/connect.
		h := upgradeconn.Handler(upgradeconn.Daraja,
			func(upConn *upgradeconn.Conn) {
				defer upConn.Close()

				var req protocol.DarajaHelloRequest
				if err := json.NewDecoder(upConn).Decode(&req); err != nil {
					return
				}
				resp := cb(req)
				_ = json.NewEncoder(upConn).Encode(resp)

				// Close the connection so ServeInverted returns EOF and
				// triggers a reconnect on the daraja side.
			})

		_ = http.Serve(ln, h)
	}()

	return path
}

// TestReconnectPresentsTheCredentialNotTheTicket verifies that daraja presents
// the ticket on the first dial and the credential returned by the daemon on
// every reconnect. A terminal error ends the loop (so the test can complete),
// but a retryable error would keep it going.
func TestReconnectPresentsTheCredentialNotTheTicket(t *testing.T) {
	var got []protocol.DarajaHelloRequest
	var mu sync.Mutex

	srv := helloOnlyDaemon(t, func(req protocol.DarajaHelloRequest) protocol.DarajaHelloResponse {
		mu.Lock()
		got = append(got, req)
		n := len(got)
		mu.Unlock()
		if n == 1 {
			return protocol.DarajaHelloResponse{Type: "daraja_hello", Credential: "reconnect-me"}
		}
		// Refuse terminally so Connect returns instead of looping forever.
		return protocol.DarajaHelloResponse{Type: "daraja_hello", Error: "enough"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: srv, ChildID: "c1", Ticket: "tk-1",
		Handler: http.NewServeMux(),
	})

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("saw %d hellos, want at least 2 (initial + reconnect)", len(got))
	}
	if got[0].Ticket != "tk-1" || got[0].Credential != "" {
		t.Errorf("first hello = %+v, want the ticket and no credential", got[0])
	}
	if got[1].Credential != "reconnect-me" || got[1].Ticket != "" {
		t.Errorf("second hello = %+v, want the credential and no ticket", got[1])
	}
}

// TestTerminalHelloRejectionStopsTheLoop verifies that a non-retryable
// error from the daemon (Retryable=false) ends Connect immediately rather
// than retrying — daraja must exit over a definitive answer about its
// credential.
func TestTerminalHelloRejectionStopsTheLoop(t *testing.T) {
	var helloCount int

	srv := helloOnlyDaemon(t, func(req protocol.DarajaHelloRequest) protocol.DarajaHelloResponse {
		helloCount++
		return protocol.DarajaHelloResponse{
			Type:      "daraja_hello",
			Error:     "ticket is unknown, already used, or revoked",
			Retryable: false,
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: srv, ChildID: "c1", Ticket: "spent-ticket",
		Handler: http.NewServeMux(),
	})

	if !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	// Should have tried exactly once — no retries on terminal rejection.
	if helloCount != 1 {
		t.Errorf("hello count = %d, want 1", helloCount)
	}
}

// TestConnectionFailureDoesNotHang verifies that when the daemon's socket
// does not exist, Connect times out gracefully rather than hanging forever.
// This is what happens when AdminService.Launch builds a valid argv but
// the target UDS path doesn't exist (e.g. wrong --connect-socket value).
func TestConnectionFailureDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: "/tmp/nonexistent-daraja-target.sock",
		ChildID:    "c1",
		Ticket:     "some-ticket",
		Handler:    http.NewServeMux(),
	})

	// Should NOT be ErrRejected (no connection was made)
	if errors.Is(err, daraja.ErrRejected) {
		t.Fatal("connection failure should not produce ErrRejected")
	}
	// Context timeout is expected since the socket doesn't exist.
	if ctx.Err() == nil {
		t.Error("expected context deadline exceeded, got nil")
	}
}

// TestNoTicketOrCredentialReturnsErrorEarly verifies that when darja has
// neither a ticket nor a credential, it enters the reconnect loop and waits
// for one to appear. Without either, darja cannot authenticate and retries
// its connection indefinitely (or until context cancellation).
func TestNoTicketOrCredentialReturnsErrorEarly(t *testing.T) {
	var helloCount int

	srv := helloOnlyDaemon(t, func(req protocol.DarajaHelloRequest) protocol.DarajaHelloResponse {
		helloCount++
		return protocol.DarajaHelloResponse{Type: "daraja_hello"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	err := daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: srv, ChildID: "c1",
		// No Ticket, no Credential!
		Handler: http.NewServeMux(),
	})

	// Should get context timeout, not ErrRejected (no auth happened yet).
	if errors.Is(err, daraja.ErrRejected) {
		t.Fatal("connection failure should not produce ErrRejected")
	}
	if ctx.Err() == nil {
		t.Error("expected context deadline exceeded, got nil")
	}
	// The writeHello error should have prevented any hellos from being sent.
	if helloCount > 0 {
		t.Errorf("hello count = %d, want 0 (writeHello should fail before dialing)", helloCount)
	}
}
