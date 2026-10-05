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
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"connectrpc.com/connect"

	"github.com/multigres/testkit/assert"
)

// shortTempDir returns a SHORT-lived temp dir whose path cannot exceed the
// kernel UDS (sun_path) limit even when the test name is long — macOS caps it
// at 104 bytes, and t.TempDir() embeds the test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rafiki")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// udsHTTPClient dials the given unix socket and speaks h2c over it, carrying
// the current protocol epoch the daemon's gate requires (epochClient).
func udsHTTPClient(sock string) *http.Client {
	return epochClient(&http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}})
}

func TestServeConnectUDSAnswersRPCs(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	// NewServer takes a HistoryLoader; nil is fine because this test never
	// calls GetHistory. GetChild fails closed with no lister, which is the
	// error we assert on — proving the ROUTE exists, which is the point.
	srv := connectapi.NewServer(nil)

	ln, err := serveConnectUDS(ctx, srv, nil, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	if fi, statErr := os.Stat(sock); statErr != nil {
		t.Fatalf("socket not created: %v", statErr)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	_, err = client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"}))
	c.Error(err, "want an error from GetChild with no lister wired")
	// The critical assertion: a REAL Connect error, not the CodeUnimplemented
	// that a missing route produces. Unimplemented here means the handler was
	// never mounted.
	c.NotEq(connect.CodeUnimplemented, connect.CodeOf(err), "route not mounted: got CodeUnimplemented (%v)", err)
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
	c := assert.NewAborting(t)
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
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
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
		c.False(id == nil || id.UserID != want.UserID || id.Username != want.Username, "identity resolved over UDS = %+v, want %+v", id, want)
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
	c := assert.NewAborting(t)
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
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_stale_or_wrong_daemon")
	_, err = client.GetRateLimitStatus(ctx, req)
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "GetRateLimitStatus with an unrecognized token: %v, want CodeUnauthenticated", err)
	c.StrContains(err.Error(), "invalid auth token", "message = %q, want the framed handshake's wording", err)
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
	c := assert.NewAborting(t)
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
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})
	req.Header().Set("Authorization", "Bearer rfk_good")
	_, err = client.GetRateLimitStatus(ctx, req)
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "GetRateLimitStatus during a store outage: %v, want CodeUnavailable", err)
	c.NotStrContains(err.Error(), "connection refused", "store error text leaked to the caller: %q", err)
	c.StrContains(err.Error(), "identity store unavailable", "message = %q, want the fixed outage message", err)
	select {
	case id := <-seen:
		t.Fatalf("the handler ran during a store outage (identity %+v)", id)
	default:
	}
}

// No credential at all stays anonymous — the unchanged half of the rule. The
// socket decided admission; a credential was never the price of entry.
func TestServeConnectUDSNoCredentialIsAnonymous(t *testing.T) {
	c := assert.NewAborting(t)
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
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	if _, err := client.GetRateLimitStatus(ctx, connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetRateLimitStatus with no credential: %v, want the call admitted", err)
	}
	select {
	case id := <-seen:
		c.Nil(id, "a credential-less request carried identity")
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never reached")
	}
}

func TestServeConnectUDSRefusesALiveSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := serveConnectUDS(ctx, connectapi.NewServer(nil), nil, newStreamRegistry(), sock)
	assert.NewAborting(t).NoError(err, "first serveConnectUDS")
	defer ln.Close()

	if _, err := serveConnectUDS(ctx, connectapi.NewServer(nil), nil, newStreamRegistry(), sock); err == nil {
		t.Fatal("want the second bind on a live socket to be refused")
	}
}

// TestServeConnectUDSLoginReachesBeginLoginWithUnknownBearer pins the mount
// decision: Login is wired on the UDS with NO interceptors — not through
// connectControlRoute, whose policy gate fail-closes every non-Control
// procedure to userOnly. So a request carrying an UNKNOWN bearer (refused as
// Unauthenticated on any Control verb) reaches BeginLogin and gets its own
// answer — FailedPrecondition while login is unconfigured, never
// Unauthenticated: Login exists for exactly the credential-less caller.
func TestServeConnectUDSLoginReachesBeginLoginWithUnknownBearer(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No login backend wired: BeginLogin must answer FailedPrecondition —
	// the error proves the call reached the handler (a missing route would be
	// CodeUnimplemented and a dropped interceptor CodeUnauthenticated).
	srv := connectapi.NewServer(nil)
	auth := server.NewUserTokenAuth(nil, "unused-child-secret", time.Minute)

	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	client := rafikiv1connect.NewLoginClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.BeginLoginRequest{RedirectPort: 8080, ClientHost: "laptop"})
	req.Header().Set("Authorization", "Bearer definitely-not-a-token")
	_, err = client.BeginLogin(ctx, req)
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err),
		"an unknown bearer must reach BeginLogin: %v", err)
	c.StrContains(err.Error(), "OIDC login is not configured", "BeginLogin message")

	// The interceptors were not dropped from the plane: the SAME unknown
	// bearer on a Control verb is still refused as Unauthenticated.
	ctrl := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	creq := connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"})
	creq.Header().Set("Authorization", "Bearer definitely-not-a-token")
	_, cerr := ctrl.GetChild(ctx, creq)
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(cerr),
		"the same unknown bearer on Control: %v", cerr)
}
