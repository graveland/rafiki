// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// shortTempDir returns a SHORT-lived temp dir whose path cannot exceed the
// kernel UDS (sun_path) limit even when the test name is long — macOS caps it
// at 104 bytes, and t.TempDir() embeds the test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

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
// flows from a UDS request's Authorization header through to a handler. err
// turns Authenticate into a store outage — an error that is NOT
// users.ErrNotFound, so it must never read as a bad credential.
type stubUserStore struct {
	users.Store
	token string
	id    users.Identity
	err   error
}

func (s stubUserStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if s.err != nil {
		return users.Identity{}, s.err
	}
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

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and this
	// test name pushes t.TempDir over it.
	sock := filepath.Join(shortTempDir(t), "s")
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

// A credential that does not resolve must be REFUSED, exactly as the framed
// unix socket refuses the same credential: Unauthenticated, with the framed
// handshake's wording — never a silent downgrade to anonymous. The planes
// must agree, because one that swallows an invalid credential and one that
// refuses it answer the same operator differently depending on which plane
// the request took.
func TestServeConnectUDSUnknownTokenIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := stubUserStore{token: "rfk_good", id: users.Identity{UserID: "u1"}}
	auth := server.NewUserTokenAuth(store, "unused-child-secret", time.Minute)

	srv := connectapi.NewServer(nil)
	seen := make(chan *server.Identity, 1)
	srv.SetQuotaReader(recordingQuotaReader{seen: seen})

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and this
	// test name pushes t.TempDir over it.
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_stale_or_wrong_daemon")
	_, err = client.GetRateLimitStatus(ctx, req)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("GetRateLimitStatus with an unrecognized token: %v, want CodeUnauthenticated", err)
	}
	if !strings.Contains(err.Error(), "invalid auth token") {
		t.Fatalf("message = %q, want the framed handshake's wording", err)
	}
	select {
	case id := <-seen:
		t.Fatalf("the handler ran despite an unrecognized credential (identity %+v)", id)
	default:
	}
}

// A store outage is an UNAVAILABLE refusal, never a bad-credential answer and
// never the store's own error text — the same rule the framed handshake and
// the proxy-face middleware follow, for the same reason: a 401-shaped answer
// makes clients discard working tokens, and a pgx error carries the DSN.
func TestServeConnectUDSStoreOutageIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := stubUserStore{err: errors.New("connection refused")}
	auth := server.NewUserTokenAuth(store, "unused-child-secret", time.Minute)

	srv := connectapi.NewServer(nil)
	seen := make(chan *server.Identity, 1)
	srv.SetQuotaReader(recordingQuotaReader{seen: seen})

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and this
	// test name pushes t.TempDir over it.
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_good")
	_, err = client.GetRateLimitStatus(ctx, req)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("GetRateLimitStatus during a store outage: %v, want CodeUnavailable", err)
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("store error text leaked to the caller: %q", err)
	}
	if !strings.Contains(err.Error(), "identity store unavailable") {
		t.Fatalf("message = %q, want the fixed outage message", err)
	}
	select {
	case id := <-seen:
		t.Fatalf("the handler ran during a store outage (identity %+v)", id)
	default:
	}
}

// No credential at all stays anonymous — the unchanged half of the rule. The
// socket decided admission; a credential was never the price of entry.
func TestServeConnectUDSNoCredentialIsAnonymous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := stubUserStore{token: "rfk_good", id: users.Identity{UserID: "u1"}}
	auth := server.NewUserTokenAuth(store, "unused-child-secret", time.Minute)

	srv := connectapi.NewServer(nil)
	seen := make(chan *server.Identity, 1)
	srv.SetQuotaReader(recordingQuotaReader{seen: seen})

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and this
	// test name pushes t.TempDir over it.
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	if _, err := client.GetRateLimitStatus(ctx, connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetRateLimitStatus with no credential: %v, want the call admitted", err)
	}
	select {
	case id := <-seen:
		if id != nil {
			t.Fatalf("a credential-less request carried identity %+v, want nil", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never reached")
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
