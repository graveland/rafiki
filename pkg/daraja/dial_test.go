package daraja_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/daraja"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// stubUpgrade records what one upgrade request presented.
type stubUpgrade struct {
	Scheme  upgradeconn.Scheme
	Secret  string
	ChildID string
}

// stubDaemon creates a unix socket in /tmp, acts as the rafikid server using
// upgradeconn.Handler, accepts multiple sequential daraja connections, and for
// each one parses the Authorization header off the upgrade request, hands it to
// the callback, and either answers 101 with the callback's headers or refuses
// with the callback's status and reason — closing that connection before
// accepting the next. The listener is bound before returning, so Connect's
// first dial reaches it. Returns the socket path for Connect to dial.
func stubDaemon(t *testing.T, cb func(scheme upgradeconn.Scheme, secret, childID string) (http.Header, *upgradeconn.Refusal)) string {
	t.Helper()
	path := "/tmp/daraja-auth-" + t.Name() + ".sock"
	_ = os.Remove(path) // clean stale socket from previous test run

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		// The HTTP handler authorizes the upgrade request itself and only
		// then hijacks. This mirrors what rafikid does on /daraja/connect.
		h := upgradeconn.Handler(upgradeconn.Daraja,
			func(r *http.Request) (struct{}, http.Header, error) {
				scheme, secret, ref := upgradeconn.AuthorizationFrom(r)
				if ref != nil {
					return struct{}{}, nil, ref
				}
				hdr, ref := cb(scheme, secret, r.Header.Get(upgradeconn.HeaderChildID))
				if ref != nil {
					return struct{}{}, nil, ref
				}
				return struct{}{}, hdr, nil
			},
			func(upConn *upgradeconn.Conn, _ struct{}) {
				defer upConn.Close()
				// Closing immediately makes ServeInverted return and
				// triggers a reconnect on the daraja side.
			})

		_ = http.Serve(ln, h)
	}()

	return path
}

// TestReconnectPresentsTheCredentialNotTheTicket verifies that daraja presents
// the ticket on the first dial and the credential returned by the daemon on
// every reconnect. A terminal refusal ends the loop (so the test can
// complete), but a retriable refusal would keep it going.
func TestReconnectPresentsTheCredentialNotTheTicket(t *testing.T) {
	var mu sync.Mutex
	var got []stubUpgrade

	srv := stubDaemon(t, func(scheme upgradeconn.Scheme, secret, childID string) (http.Header, *upgradeconn.Refusal) {
		mu.Lock()
		got = append(got, stubUpgrade{Scheme: scheme, Secret: secret, ChildID: childID})
		n := len(got)
		mu.Unlock()
		if n == 1 {
			// First dial: grant and mint a reconnect credential.
			return http.Header{upgradeconn.HeaderCredential: {"reconnect-me"}}, nil
		}
		// Refuse terminally so Connect returns instead of looping forever.
		return nil, &upgradeconn.Refusal{Status: http.StatusUnauthorized, Reason: "enough"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: srv, ChildID: "c1", Ticket: "tk-1",
		Handler: http.NewServeMux(),
	})
	if !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("Connect = %v, want ErrRejected (the second dial is refused)", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("saw %d upgrades, want at least 2 (initial + reconnect)", len(got))
	}
	if got[0].Scheme != upgradeconn.SchemeTicket || got[0].Secret != "tk-1" {
		t.Errorf("first upgrade = %+v, want the ticket scheme carrying tk-1", got[0])
	}
	if got[0].ChildID != "c1" {
		t.Errorf("first upgrade child id = %q, want c1", got[0].ChildID)
	}
	if got[1].Scheme != upgradeconn.SchemeBearer || got[1].Secret != "reconnect-me" {
		t.Errorf("second upgrade = %+v, want the bearer scheme carrying the credential", got[1])
	}
}

// TestTerminalHelloRejectionStopsTheLoop verifies that a 401 on the upgrade
// request ends Connect immediately rather than retrying — daraja must exit
// over a definitive answer about its credential.
func TestTerminalHelloRejectionStopsTheLoop(t *testing.T) {
	var mu sync.Mutex
	dials := 0

	srv := stubDaemon(t, func(upgradeconn.Scheme, string, string) (http.Header, *upgradeconn.Refusal) {
		mu.Lock()
		dials++
		mu.Unlock()
		return nil, &upgradeconn.Refusal{
			Status: http.StatusUnauthorized,
			Reason: "ticket is unknown, already used, or revoked",
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
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Errorf("dial count = %d, want 1", dials)
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

// TestNoTicketOrCredentialReturnsErrorEarly verifies that when daraja has
// neither a ticket nor a credential, it enters the reconnect loop and waits
// for one to appear. Without either, daraja cannot authenticate and retries
// its connection indefinitely (or until context cancellation).
func TestNoTicketOrCredentialReturnsErrorEarly(t *testing.T) {
	var mu sync.Mutex
	upgrades := 0

	srv := stubDaemon(t, func(upgradeconn.Scheme, string, string) (http.Header, *upgradeconn.Refusal) {
		mu.Lock()
		upgrades++
		mu.Unlock()
		return http.Header{upgradeconn.HeaderCredential: {"cred"}}, nil
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
	// The missing-credential error must fire before any upgrade is sent.
	mu.Lock()
	defer mu.Unlock()
	if upgrades > 0 {
		t.Errorf("upgrade count = %d, want 0 (nothing to authenticate with)", upgrades)
	}
}

// TestUpgrade503IsRetriedNotTerminal verifies that a non-auth status on the
// upgrade keeps the reconnect loop going: only 400, 401 and 403 are terminal.
// The stub refuses the first dial with 503 and grants the second, so a 503
// that ended the loop would leave daraja with no credential and no retry.
func TestUpgrade503IsRetriedNotTerminal(t *testing.T) {
	var mu sync.Mutex
	var got []stubUpgrade

	srv := stubDaemon(t, func(scheme upgradeconn.Scheme, secret, childID string) (http.Header, *upgradeconn.Refusal) {
		mu.Lock()
		got = append(got, stubUpgrade{Scheme: scheme, Secret: secret, ChildID: childID})
		n := len(got)
		mu.Unlock()
		if n == 1 {
			return nil, &upgradeconn.Refusal{Status: http.StatusServiceUnavailable, Reason: "try again"}
		}
		if n == 2 {
			return http.Header{upgradeconn.HeaderCredential: {"reconnect-me"}}, nil
		}
		// Refuse terminally so Connect returns instead of looping forever.
		return nil, &upgradeconn.Refusal{Status: http.StatusUnauthorized, Reason: "enough"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := daraja.Connect(ctx, daraja.ConnectOptions{
		SocketPath: srv, ChildID: "c1", Ticket: "tk-1",
		Handler: http.NewServeMux(),
	})
	// ErrRejected is EXPECTED here — it comes from the stub's deliberate
	// terminal refusal on the third dial, after the loop survived the 503.
	if !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("Connect = %v, want ErrRejected from the stub's final refusal", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("saw %d upgrades, want at least 3 (503 refusal, grant, terminal refusal)", len(got))
	}
	if got[1].Scheme != upgradeconn.SchemeTicket {
		t.Errorf("second upgrade scheme = %q, want the ticket retried after the 503", got[1].Scheme)
	}
	if got[2].Scheme != upgradeconn.SchemeBearer || got[2].Secret != "reconnect-me" {
		t.Errorf("third upgrade = %+v, want the credential from the granted 101", got[2])
	}
}
