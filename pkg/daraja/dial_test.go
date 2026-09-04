package daraja_test

import (
	"context"
	"encoding/json"
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
