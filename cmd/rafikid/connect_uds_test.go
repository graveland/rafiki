// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"connectrpc.com/connect"
)

// udsHTTPClient dials the given unix socket and speaks h2c over it.
func udsHTTPClient(sock string) *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func TestServeConnectUDSAnswersRPCs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	// NewServer takes a HistoryLoader; nil is fine because this test never
	// calls GetHistory. GetChild fails closed with no lister, which is the
	// error we assert on — proving the ROUTE exists, which is the point.
	srv := connectapi.NewServer(nil)

	ln, err := serveConnectUDS(ctx, srv, nil, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	if fi, statErr := os.Stat(sock); statErr != nil {
		t.Fatalf("socket not created: %v", statErr)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	_, err = client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"}))
	if err == nil {
		t.Fatal("want an error from GetChild with no lister wired")
	}
	// The critical assertion: a REAL Connect error, not the CodeUnimplemented
	// that a missing route produces. Unimplemented here means the handler was
	// never mounted.
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("route not mounted: got CodeUnimplemented (%v)", err)
	}
}

// stubUserStore resolves exactly one token, for proving identity actually
// flows from a UDS request's Authorization header through to a handler.
type stubUserStore struct {
	users.Store
	token string
	id    users.Identity
}

func (s stubUserStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if token != s.token {
		return users.Identity{}, users.ErrNotFound
	}
	return s.id, nil
}

// recordingQuotaReader captures the identity connectapi.Server resolved from
// ctx for the call, so the test can assert on it without needing a real
// database-backed quota store.
type recordingQuotaReader struct{ seen chan *server.Identity }

func (r recordingQuotaReader) RateLimitStatus(ctx context.Context) (connectapi.RateLimitStatus, bool, error) {
	r.seen <- server.IdentityFromContext(ctx)
	return connectapi.RateLimitStatus{}, false, nil
}

// TestServeConnectUDSResolvesIdentity is the end-to-end
// proof for the fix: a request carrying a real per-user token over the LOCAL
// UDS socket resolves to that user's identity in ctx, exactly as it already
// does on the TCP/TLS proxy face. Before this fix, GetRateLimitStatus (and
// anything else keyed on the caller's own identity) always saw a nil
// identity here, because the UDS mount had no identity resolution at all.
func TestServeConnectUDSResolvesIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	want := users.Identity{UserID: "u1", Username: "brent"}
	store := stubUserStore{token: "rfk_good", id: want}
	auth := server.NewUserTokenAuth(store, "unused-child-secret", time.Minute)

	srv := connectapi.NewServer(nil)
	seen := make(chan *server.Identity, 1)
	srv.SetQuotaReader(recordingQuotaReader{seen: seen})

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_good")
	if _, err := client.GetRateLimitStatus(ctx, req); connect.CodeOf(err) != connect.CodeNotFound {
		// NotFound is expected — recordingQuotaReader reports ok=false. Any
		// OTHER outcome means the request never reached the handler.
		t.Fatalf("GetRateLimitStatus: %v", err)
	}

	select {
	case id := <-seen:
		if id == nil || id.UserID != want.UserID || id.Username != want.Username {
			t.Fatalf("identity resolved over UDS = %+v, want %+v", id, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never reached")
	}
}

// A credential that does not resolve must proceed anonymously, never reject
// the call — the socket itself already decided admission.
func TestServeConnectUDSBadTokenIsAnonymous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := stubUserStore{token: "rfk_good", id: users.Identity{UserID: "u1"}}
	auth := server.NewUserTokenAuth(store, "unused-child-secret", time.Minute)

	srv := connectapi.NewServer(nil)
	seen := make(chan *server.Identity, 1)
	srv.SetQuotaReader(recordingQuotaReader{seen: seen})

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_stale_or_wrong_daemon")
	if _, err := client.GetRateLimitStatus(ctx, req); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetRateLimitStatus with an unrecognized token: %v, want the call admitted (NotFound from the stub reader)", err)
	}

	select {
	case id := <-seen:
		if id != nil {
			t.Fatalf("identity resolved from an unrecognized token = %+v, want nil", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never reached — an unrecognized credential must not block admission")
	}
}

func TestServeConnectUDSRefusesALiveSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := serveConnectUDS(ctx, connectapi.NewServer(nil), nil, sock)
	if err != nil {
		t.Fatalf("first serveConnectUDS: %v", err)
	}
	defer ln.Close()

	if _, err := serveConnectUDS(ctx, connectapi.NewServer(nil), nil, sock); err == nil {
		t.Fatal("want the second bind on a live socket to be refused")
	}
}
