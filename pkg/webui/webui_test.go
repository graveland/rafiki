// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"

	"github.com/multigres/testkit/assert"
)

// testPort is the port every recorder-driven test claims to be bound to; it
// never actually listens.
const testPort = 8099

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>index</html>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log('app')\n")},
	}
}

func loopbackHost(port int) string   { return fmt.Sprintf("127.0.0.1:%d", port) }
func loopbackOrigin(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

func newReq(method, target, host, origin string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// controlReq builds a Control-plane request with an explicit Host and Origin.
func controlReq(method, target, secret, origin string) *http.Request {
	r := newReq(method, target, loopbackHost(testPort), origin)
	if secret != "" {
		r.Header.Set("X-Rafiki-Web", secret)
	}
	return r
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// addSecret registers a session secret directly, standing in for the
// launch-code exchange when a test is about something else.
func addSecret(s *Server, secret string) string {
	s.sessions.mu.Lock()
	s.sessions.secrets[secret] = struct{}{}
	s.sessions.mu.Unlock()
	return secret
}

// exchange posts a launch code to /_rafiki/session and returns the status plus
// the minted secret (empty on failure).
func exchange(t *testing.T, s *Server, code string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("marshal launch code: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/_rafiki/session", bytes.NewReader(body))
	r.Host = loopbackHost(testPort)
	r.Header.Set("Origin", loopbackOrigin(testPort))
	w := serve(s, r)
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return w.Code, out.Secret
}

func fakeUpstream(t *testing.T, handle http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	return srv
}

func staticResolver(u Upstream) Resolver {
	return func() (Upstream, error) { return u, nil }
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assertNoAccessControl(t *testing.T, h http.Header) {
	t.Helper()
	for name := range h {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("response carries CORS header %q", name)
		}
	}
}

func TestWebuiHostMismatchRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	w := serve(s, newReq(http.MethodGet, "/app.js", "evil.example:8099", ""))
	ck.Eq(http.StatusForbidden, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiHostMatchServesStatic(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	for _, host := range []string{"127.0.0.1:8099", "localhost:8099"} {
		// No X-Rafiki-Web: static needs no secret.
		w := serve(s, newReq(http.MethodGet, "/app.js", host, ""))
		ck.Eq(http.StatusOK, w.Code, "status for "+host)
		ck.Eq("console.log('app')\n", w.Body.String(), "body for "+host)
	}
}

func TestWebuiSPAFallback(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	w := serve(s, newReq(http.MethodGet, "/agents/xyz", "127.0.0.1:8099", ""))
	ck.Eq(http.StatusOK, w.Code, "status")
	ck.Eq("<html>index</html>", w.Body.String(), "index.html body")
}

func TestWebuiPostForeignOriginRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	secret := addSecret(s, "foreign-origin-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, "http://evil.example"))
	ck.Eq(http.StatusForbidden, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiPostMissingOriginRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	secret := addSecret(s, "missing-origin-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, ""))
	ck.Eq(http.StatusForbidden, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiMissingSecretRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", "", loopbackOrigin(testPort)))
	ck.Eq(http.StatusUnauthorized, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiWrongSecretRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	addSecret(s, "the-real-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", "the-wrong-secret", loopbackOrigin(testPort)))
	ck.Eq(http.StatusUnauthorized, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiLaunchCodeSingleUse(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	code := s.LaunchCode()
	ck.NotEmpty(code, "launch code")

	status, secret := exchange(t, s, code)
	ck.Eq(http.StatusOK, status, "first exchange")
	ck.NotEmpty(secret, "minted secret")

	status2, _ := exchange(t, s, code)
	ck.Eq(http.StatusGone, status2, "second exchange")
}

func TestWebuiSessionSecretAdmitsProxy(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits int32
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, "ok")
	})
	s := New(Config{
		Port:    testPort,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})

	status, secret := exchange(t, s, s.LaunchCode())
	ck.Eq(http.StatusOK, status, "exchange status")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/Echo", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusOK, w.Code, "proxy status")
	ck.Eq("ok", w.Body.String(), "upstream body")
	ck.Eq(int32(1), atomic.LoadInt32(&hits), "upstream requests")
}

func TestWebuiProxyStripsBrowserCredentials(t *testing.T) {
	ck := assert.NewCollecting(t)
	var mu sync.Mutex
	var gotAuth, gotCookie, gotWeb string
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotWeb = r.Header.Get("X-Rafiki-Web")
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	})
	s := New(Config{
		Port:    testPort,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})
	secret := addSecret(s, "strip-secret")

	r := controlReq(http.MethodPost, "/rafiki.v1.Control/Echo", secret, loopbackOrigin(testPort))
	r.Header.Set("Authorization", "Bearer browser-token")
	r.Header.Set("Cookie", "sid=browser-cookie")
	_ = serve(s, r)

	mu.Lock()
	defer mu.Unlock()
	ck.Eq("", gotAuth, "Authorization stripped")
	ck.Eq("", gotCookie, "Cookie stripped")
	ck.Eq("", gotWeb, "X-Rafiki-Web stripped")
}

func TestWebuiProxyFlushesStreamMessages(t *testing.T) {
	ck := assert.NewCollecting(t)
	// A declared Content-Length keeps the reverse proxy out of its automatic
	// flush path for unknown-length responses, so only FlushInterval=-1 flushes
	// the chunk while the upstream is still blocked.
	const bodyLen = 100
	release := make(chan struct{})
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(bodyLen))
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "chunk-1\n"); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, strings.Repeat("x", bodyLen-len("chunk-1\n")))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	ck.NoError(err, "listen")
	port := ln.Addr().(*net.TCPAddr).Port

	s := New(Config{
		Port:    port,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})
	secret := addSecret(s, "flush-secret")
	srv := &http.Server{Handler: s.Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/rafiki.v1.Control/StreamEvents", loopbackHost(port)), nil)
	ck.NoError(err, "build request")
	req.Header.Set("X-Rafiki-Web", secret)
	req.Header.Set("Origin", loopbackOrigin(port))

	// Do and the read run together in a goroutine so that a proxy which never
	// flushes fails via the 5s arm instead of blocking the test forever.
	const want = "chunk-1\n"
	type result struct {
		chunk string
		err   error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, len(want))
		_, err = io.ReadFull(resp.Body, buf)
		got <- result{chunk: string(buf), err: err}
	}()
	select {
	case r := <-got:
		ck.NoError(r.err, "read first chunk")
		ck.Eq(want, r.chunk, "first chunk arrived before release")
	case <-time.After(5 * time.Second):
		ck.Fail("the proxy never flushed the first stream chunk")
	}
	close(release)
}

func TestWebuiLoginServiceNotProxied(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits int32
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, "ok")
	})
	s := New(Config{
		Port:    testPort,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})
	secret := addSecret(s, "login-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Login/BeginLogin", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusNotFound, w.Code, "status")
	ck.Eq(int32(0), atomic.LoadInt32(&hits), "upstream requests")
}

func TestWebuiNoCredentialAnswersUnauthenticated(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits int32
	fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	})
	s := New(Config{
		Port:   testPort,
		Assets: testAssets(),
		Resolve: func() (Upstream, error) {
			return Upstream{}, fmt.Errorf("profile resolution: %w", ErrNoCredential)
		},
	})
	secret := addSecret(s, "no-credential-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusUnauthorized, w.Code, "status")
	ck.Eq("", w.Header().Get(gatedHeader), "no gate header")
	var body errorBody
	ck.NoError(json.Unmarshal(w.Body.Bytes(), &body), "decode body")
	ck.Eq("unauthenticated", body.Code, "body code")
	ck.Eq(int32(0), atomic.LoadInt32(&hits), "upstream requests")
}

func TestWebuiResolverErrorNotCached(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits, calls int32
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, "ok")
	})
	s := New(Config{
		Port:   testPort,
		Assets: testAssets(),
		Resolve: func() (Upstream, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return Upstream{}, errors.New("resolver not ready")
			}
			return Upstream{Client: &http.Client{}, BaseURL: up.URL}, nil
		},
	})
	secret := addSecret(s, "resolve-retry-secret")

	w1 := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/A", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusServiceUnavailable, w1.Code, "first request status")

	w2 := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/B", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusOK, w2.Code, "second request status")
	ck.Eq(int32(1), atomic.LoadInt32(&hits), "upstream requests")
}

func TestWebuiUpstreamConnectErrorMapped(t *testing.T) {
	ck := assert.NewCollecting(t)
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("nope"))
	})
	s := New(Config{
		Port:   testPort,
		Assets: testAssets(),
		Resolve: staticResolver(Upstream{
			Client:  &http.Client{Transport: rt},
			BaseURL: "http://upstream.invalid",
		}),
	})
	secret := addSecret(s, "connect-error-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusBadRequest, w.Code, "status")
	ck.Eq("", w.Header().Get(gatedHeader), "no gate header")
	var body errorBody
	ck.NoError(json.Unmarshal(w.Body.Bytes(), &body), "decode body")
	ck.Eq("failed_precondition", body.Code, "body code")
}

func TestWebuiLazyResolveDoesNotOverwriteSetUpstream(t *testing.T) {
	ck := assert.NewCollecting(t)
	var aHits, bHits int32
	a := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&aHits, 1)
		_, _ = io.WriteString(w, "a")
	})
	b := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&bHits, 1)
		_, _ = io.WriteString(w, "b")
	})
	s := New(Config{Port: testPort, Assets: testAssets()})
	// A login (SetUpstream) lands while the first request is resolving; the
	// lazy resolve must lose to it rather than stomp it.
	s.cfg.Resolve = func() (Upstream, error) {
		s.SetUpstream(Upstream{Client: &http.Client{}, BaseURL: b.URL})
		return Upstream{Client: &http.Client{}, BaseURL: a.URL}, nil
	}
	secret := addSecret(s, "cas-secret")

	w := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusOK, w.Code, "status")
	ck.Eq("b", w.Body.String(), "set upstream wins")
	ck.Eq(int32(1), atomic.LoadInt32(&bHits), "upstream b requests")
	ck.Eq(int32(0), atomic.LoadInt32(&aHits), "upstream a requests")
}

func TestWebuiSetUpstreamSwaps(t *testing.T) {
	ck := assert.NewCollecting(t)
	var aHits, bHits int32
	a := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&aHits, 1)
		_, _ = io.WriteString(w, "a")
	})
	b := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&bHits, 1)
		_, _ = io.WriteString(w, "b")
	})
	s := New(Config{Port: testPort, Assets: testAssets()})
	secret := addSecret(s, "swap-secret")

	s.SetUpstream(Upstream{Client: &http.Client{}, BaseURL: a.URL})
	_ = serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))

	s.SetUpstream(Upstream{Client: &http.Client{}, BaseURL: b.URL})
	_ = serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))

	ck.Eq(int32(1), atomic.LoadInt32(&aHits), "upstream a requests")
	ck.Eq(int32(1), atomic.LoadInt32(&bHits), "upstream b requests")
}

func TestWebuiNoCORSHeaders(t *testing.T) {
	ck := assert.NewCollecting(t)
	// The upstream deliberately emits CORS, the gate marker and a cookie: none
	// may reach the browser.
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set(gatedHeader, "refused")
		w.Header().Set("Set-Cookie", "sid=upstream")
		_, _ = io.WriteString(w, "ok")
	})
	s := New(Config{
		Port:    testPort,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})
	secret := addSecret(s, "cors-secret")

	pre := serve(s, newReq(http.MethodOptions, "/rafiki.v1.Control/X", loopbackHost(testPort), loopbackOrigin(testPort)))
	ck.Eq(http.StatusMethodNotAllowed, pre.Code, "preflight status")
	assertNoAccessControl(t, pre.Header())

	proxied := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusOK, proxied.Code, "proxied status")
	ck.Eq("ok", proxied.Body.String(), "proxied body")
	assertNoAccessControl(t, proxied.Header())
	ck.Eq("", proxied.Header().Get(gatedHeader), "upstream gate marker stripped")
	ck.Eq("", proxied.Header().Get("Set-Cookie"), "upstream Set-Cookie stripped")
}

func TestWebuiControlHostMismatchRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	secret := addSecret(s, "control-host-secret")

	r := newReq(http.MethodPost, "/rafiki.v1.Control/X", "evil.example:8099", loopbackOrigin(testPort))
	r.Header.Set("X-Rafiki-Web", secret)
	w := serve(s, r)
	ck.Eq(http.StatusForbidden, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
}

func TestWebuiDevAssetsStripsResponseHeaders(t *testing.T) {
	ck := assert.NewCollecting(t)
	// The fake Vite dev server emits CORS, the gate marker and a cookie: the
	// dev-assets proxy must strip them the same way the Control proxy does.
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set(gatedHeader, "refused")
		w.Header().Set("Set-Cookie", "sid=vite")
		_, _ = io.WriteString(w, "<html>dev</html>")
	})
	base, err := url.Parse(up.URL)
	ck.NoError(err, "parse dev-assets url")
	s := New(Config{Port: testPort, DevAssets: base})

	w := serve(s, newReq(http.MethodGet, "/app.js", loopbackHost(testPort), ""))
	ck.Eq(http.StatusOK, w.Code, "status")
	ck.Eq("<html>dev</html>", w.Body.String(), "dev body")
	assertNoAccessControl(t, w.Header())
	ck.Eq("", w.Header().Get(gatedHeader), "upstream gate marker stripped")
	ck.Eq("", w.Header().Get("Set-Cookie"), "upstream Set-Cookie stripped")
}

func TestWebuiSecurityHeaders(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	w := serve(s, newReq(http.MethodGet, "/app.js", loopbackHost(testPort), ""))
	ck.Eq(contentSecurityPolicy, w.Header().Get("Content-Security-Policy"), "CSP")
	ck.Eq("nosniff", w.Header().Get("X-Content-Type-Options"), "nosniff")
	ck.Eq("no-referrer", w.Header().Get("Referrer-Policy"), "referrer policy")
}

func TestWebuiHTTPServerHasNoWriteTimeout(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})

	hs := s.HTTPServer()
	ck.Eq(time.Duration(0), hs.WriteTimeout, "WriteTimeout")
	ck.Greater(time.Duration(0), hs.ReadHeaderTimeout, "ReadHeaderTimeout")
	ck.NotNil(hs.Handler, "handler")
}

func TestWebuiHasUI(t *testing.T) {
	ck := assert.NewCollecting(t)
	// Pin the rule against in-memory trees only: the real embed is build output
	// (a `make web` before `go test` flips it), so it is never asserted here.
	ck.True(hasUI(testAssets()), "a tree with index.html reports a UI")
	ck.False(hasUI(fstest.MapFS{}), "a tree without index.html reports no UI")
}
