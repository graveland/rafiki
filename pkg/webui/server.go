// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream is the resolved daemon the proxy forwards Control traffic to.
type Upstream struct {
	// Client carries the bearer and the protocol epoch (cmd/rafiki's
	// bearerTransport); the proxy uses its Transport.
	Client *http.Client
	// BaseURL is the daemon base URL, e.g. "http://connect.rafiki.invalid"
	// (a UDS profile) or "https://host:port".
	BaseURL string
}

// ErrNoCredential is returned (wrapped or bare) by a Resolver when the profile
// names a remote daemon and holds no token. The proxy answers it as Connect
// Unauthenticated WITHOUT dialling.
var ErrNoCredential = errors.New("rafiki web: profile has no credential")

// Resolver resolves the upstream daemon. It is called lazily on the first
// proxied request and its result cached; an error is never cached.
type Resolver func() (Upstream, error)

// Config configures a Server.
type Config struct {
	Port      int      // the bound port, used by the Host/Origin checks
	Assets    fs.FS    // static files; nil means DevAssets must be set
	DevAssets *url.URL // when non-nil, static requests are reverse-proxied here (Vite dev server)
	Resolve   Resolver // called lazily on the first proxied request, then cached
}

// Server is the rafiki web UI handler and session bookkeeper.
type Server struct {
	cfg      Config
	upstream atomic.Pointer[Upstream]
	resolveM sync.Mutex
	sessions *sessionStore

	extrasM sync.Mutex
	extras  []extraRoute
}

type extraRoute struct {
	pattern       string
	handler       http.Handler
	requireSecret bool
}

// New builds a Server for cfg.
func New(cfg Config) *Server {
	return &Server{
		cfg:      cfg,
		sessions: newSessionStore(),
	}
}

// SetUpstream replaces the cached upstream atomically (used after a login).
func (s *Server) SetUpstream(u Upstream) { s.upstream.Store(&u) }

// Handle registers an extra handler under the gate (used for /_rafiki/login/*
// by a later task). requireSecret=false drops the session-secret check (for
// /oidc/callback); the Host/Origin checks still apply, which for a GET
// navigation is exactly the Host check.
func (s *Server) Handle(pattern string, h http.Handler, requireSecret bool) {
	s.extrasM.Lock()
	s.extras = append(s.extras, extraRoute{pattern: pattern, handler: h, requireSecret: requireSecret})
	s.extrasM.Unlock()
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_rafiki/session", s.serveSession)

	s.extrasM.Lock()
	extras := append([]extraRoute(nil), s.extras...)
	s.extrasM.Unlock()
	for _, e := range extras {
		if e.requireSecret {
			mux.Handle(e.pattern, s.requireSecret(e.handler))
		} else {
			mux.Handle(e.pattern, e.handler)
		}
	}

	mux.Handle("/rafiki.v1.Control/", s.requireSecret(http.HandlerFunc(s.serveControl)))
	mux.Handle("/rafiki.v1.Login/", s.requireSecret(http.HandlerFunc(serveLoginNotProxied)))
	mux.Handle("/", http.HandlerFunc(s.serveStatic))

	return s.gate(mux)
}

// HTTPServer returns an *http.Server with ReadHeaderTimeout 10s and
// IdleTimeout 120s, and NO WriteTimeout (the UI streams responses).
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// currentUpstream returns the cached upstream, resolving it under a mutex on
// the first proxied request. A resolve error is not cached, so the next
// request tries again.
func (s *Server) currentUpstream() (Upstream, error) {
	if u := s.upstream.Load(); u != nil {
		return *u, nil
	}
	s.resolveM.Lock()
	defer s.resolveM.Unlock()
	if u := s.upstream.Load(); u != nil {
		return *u, nil
	}
	if s.cfg.Resolve == nil {
		return Upstream{}, errors.New("rafiki web: no upstream resolver configured")
	}
	u, err := s.cfg.Resolve()
	if err != nil {
		return Upstream{}, err
	}
	// Compare-and-swap so a lazy resolve can never overwrite a concurrent
	// SetUpstream (a login landing while the first request was in flight). If
	// the swap loses, keep what was set.
	if s.upstream.CompareAndSwap(nil, &u) {
		return u, nil
	}
	if existing := s.upstream.Load(); existing != nil {
		return *existing, nil
	}
	return u, nil
}
