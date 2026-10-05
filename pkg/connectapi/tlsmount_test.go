// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/execpool"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// tokenRoundTripper is the client half of cmd/rafiki's bearerTransport: it
// carries the credential AND the protocol epoch the mounted face now requires.
type tokenRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	r.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	return t.base.RoundTrip(r)
}

// oneUserStore answers exactly one token, so the face's middleware resolves a
// real identity without a database.
type oneUserStore struct {
	token string
	id    users.Identity
}

func (s oneUserStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if token == s.token {
		return s.id, nil
	}
	return users.Identity{}, users.ErrNotFound
}
func (s oneUserStore) Create(context.Context, users.NewUser) (users.User, string, error) {
	return users.User{}, "", users.ErrNotFound
}
func (s oneUserStore) List(context.Context, bool, int) ([]users.User, error) { return nil, nil }
func (s oneUserStore) Delete(context.Context, string) error                  { return nil }
func (s oneUserStore) CountActive(context.Context) (int, error)              { return 1, nil }
func (s oneUserStore) LookupUsername(context.Context, string) (string, error) {
	panic("unused")
}
func (s oneUserStore) SetEmail(context.Context, string, string) (users.User, error) {
	return users.User{}, users.ErrNotFound
}
func (s oneUserStore) MintToken(context.Context, string, users.NewToken) (users.Token, string, error) {
	return users.Token{}, "", users.ErrNotFound
}
func (s oneUserStore) ListTokens(context.Context, string, bool) ([]users.Token, error) {
	return nil, nil
}
func (s oneUserStore) GetToken(context.Context, string) (users.Token, error) {
	return users.Token{}, users.ErrNotFound
}
func (s oneUserStore) RevokeToken(context.Context, string) (users.Token, error) {
	return users.Token{}, users.ErrNotFound
}

// identityLifecycle records the identity the face's middleware put on the
// request context by the time a Connect handler reached the lifecycle seam.
type identityLifecycle struct{ saw *server.Identity }

func (l *identityLifecycle) Spawn(ctx context.Context, _ connectapi.SpawnParams) (string, error) {
	l.saw = server.IdentityFromContext(ctx)
	return "c_new", nil
}

func (l *identityLifecycle) Kill(context.Context, string, time.Duration, time.Duration) (connectapi.KillOutcome, error) {
	return connectapi.KillOutcome{}, nil
}

func (l *identityLifecycle) Close(context.Context, string) error { return nil }

func (l *identityLifecycle) SetBudget(context.Context, string, float64) error { return nil }

func (l *identityLifecycle) SetRouting(context.Context, string, string) (string, error) {
	return "", nil
}

const mountTestToken = "s3cret-user-token"

// setupTLSMount reproduces cmd/rafikid's remote surface: proxy.go composes the
// Connect routes into server.Handler and mounts them inside the face's
// UserTokenAuth middleware, and main.go serves that whole mux at "/" on the
// shared TLS listener.
//
// http/1.1 ALPN only, matching execpool.ALPNProtocols — httptest advertises h2
// only when EnableHTTP2 is set, and leaving it off is what makes this exercise
// the protocol the real listener actually negotiates.
func setupTLSMount(t *testing.T, src connectapi.EventSource, lc connectapi.ChildLifecycle) *httptest.Server {
	t.Helper()

	if got := execpool.ALPNProtocols; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("ALPNProtocols = %v; this test pins the cockpit to what the shared listener negotiates", got)
	}

	s := connectapi.NewServer(nil)
	s.SetChildResolver(fakeResolver{})
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{ChildID: "c_1"}}})
	s.SetLineage(&fakeLineage{depth: make(map[string]map[string]int), labels: make(map[string]map[string]string)})
	s.SetEventLog(eventlog.NewMemory())
	if src != nil {
		s.SetEventSource(src)
	}
	if lc != nil {
		s.SetChildLifecycle(lc)
	}

	store := oneUserStore{token: mountTestToken, id: users.Identity{UserID: "u1", Username: "brent"}}
	auth := server.NewUserTokenAuth(store, "", server.DefaultAuthCacheTTL)

	h := &server.Handler{}
	h.ControlPath, h.Control = s.Routes()
	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	// The face's unrouted-request handler. net/http answers an unrouted path
	// with a bodiless 404, which Connect reports as CodeUnimplemented.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "", http.StatusNotFound)
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.StartTLS() // EnableHTTP2 deliberately unset: http/1.1 ALPN
	t.Cleanup(srv.Close)
	return srv
}

func tlsClient(srv *httptest.Server, token string) rafikiv1connect.ControlClient {
	base := srv.Client()
	base.Transport = tokenRoundTripper{base: base.Transport, token: token}
	return rafikiv1connect.NewControlClient(base, srv.URL)
}

// The unary half: a credentialed call reaches the handler rather than the
// face's 404.
func TestCockpitUnaryWorksOverTheSharedTLSListener(t *testing.T) {
	c := assert.NewAborting(t)
	srv := setupTLSMount(t, nil, nil)
	resp, err := tlsClient(srv, mountTestToken).ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	c.NoError(err, "ListChildren over TLS")
	c.Len(resp.Msg.GetChildren(), 1, "got %d children, want 1", len(resp.Msg.GetChildren()))
}

// The assertion the remote cockpit rests on. The shared listener advertises
// http/1.1 ONLY (net/http can hijack an HTTP/1.1 connection and not an HTTP/2
// one, and /control and /executor/connect are both Upgrades), while the local
// unix socket is h2c. connect-go refuses BIDI streaming below HTTP/2;
// StreamEvents is server-streaming, which rides HTTP/1.1 chunked encoding. If
// that were wrong, a remote cockpit would connect, list children, and then
// show a permanently empty transcript.
func TestStreamEventsSurvivesHTTP11ALPN(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	srv := setupTLSMount(t, src, nil)

	src.ch <- statusEvent("c_1", "idle")

	stream, err := tlsClient(srv, mountTestToken).StreamEvents(ctx,
		connect.NewRequest(&rafikiv1.StreamEventsRequest{
			Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
		}))
	c.NoError(err, "StreamEvents over http/1.1")
	if !stream.Receive() {
		t.Fatalf("no event arrived over http/1.1: %v", stream.Err())
	}
	c.Eq("idle", stream.Msg().GetAgentStatus().GetState(), "state")

	// Prove the negotiation really was HTTP/1.1 rather than a silent h2
	// upgrade making the test vacuous.
	probe, err := srv.Client().Get(srv.URL + "/healthz")
	c.NoError(err, "probe")
	defer probe.Body.Close()
	c.Eq(1, probe.ProtoMajor, "negotiated HTTP/")
}

// The listener is public, so an uncredentialed cockpit must be refused. The
// face's middleware answers 401, which Connect surfaces as Unauthenticated —
// distinguishable from the 404-shaped Unimplemented an unrouted path gives.
func TestCockpitOverTLSRefusesAnUncredentialedCaller(t *testing.T) {
	srv := setupTLSMount(t, nil, nil)
	_, err := tlsClient(srv, "").ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "want CodeUnauthenticated, got %v (%v)", connect.CodeOf(err), err)
}

func TestCockpitOverTLSRefusesAWrongToken(t *testing.T) {
	srv := setupTLSMount(t, nil, nil)
	_, err := tlsClient(srv, "wrong").ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "want CodeUnauthenticated, got %v (%v)", connect.CodeOf(err), err)
}

// The identity the face resolved must survive into the lifecycle seam, because
// that is the only place a remote spawn can learn its owner — and the owner is
// matched by executor admission selectors. connectLifecycle.Spawn hardcoded
// users.Identity{} for as long as the socket was the only reachable mount.
func TestTheFacesIdentityReachesTheLifecycleSeam(t *testing.T) {
	c := assert.NewAborting(t)
	lc := &identityLifecycle{}
	srv := setupTLSMount(t, nil, lc)

	_, err := tlsClient(srv, mountTestToken).Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp"}))
	c.NoError(err, "Spawn")
	c.NotNil(lc.saw, "no identity on the handler's context: a remote spawn would be unowned")
	c.Eq("brent", lc.saw.Username, "username")
}

// ServeMux prefers the longest matching pattern, so the Connect prefix must
// win over the face's "/". Getting this wrong is invisible until a cockpit
// request comes back as CodeUnimplemented.
func TestTheProxyFaceDoesNotShadowTheConnectRoutes(t *testing.T) {
	c := assert.NewAborting(t)
	srv := setupTLSMount(t, nil, nil)
	if _, err := tlsClient(srv, mountTestToken).GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_1"})); err != nil {
		t.Fatalf("GetChild: %v", err)
	}
	resp, err := srv.Client().Get(srv.URL + "/nope")
	c.NoError(err, "face probe")
	defer resp.Body.Close()
	c.Eq(http.StatusNotFound, resp.StatusCode, "face status")
}
