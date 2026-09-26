// SPDX-License-Identifier: Apache-2.0

// Package childsock serves a script child's per-child Connect socket: a unix
// socket that reverse-proxies every request to the daemon's Connect control
// route with the child's own credential injected.
//
// The socket is the credential. The child process never holds a token — it
// holds a filesystem path (RAFIKI_CHILD_CONNECT) that only it can reach, and
// whatever it sends over that socket arrives at the daemon as ITSELF, via the
// same per-child secret identity every other child credential uses
// (server.ProvenanceChildToken). One identity path, one authorization path,
// for all child credentials: this package adds transport, never authority.
//
// The package is deliberately pgx-free so the executor-side daraja can link
// it (wave 4) without linking any database driver.
package childsock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SocketName is the file name served inside a child's socket directory — the
// same name the daemon's own Connect socket uses, so a client (the SDK) needs
// only the directory-or-path convention of one variable.
const SocketName = "connect.sock"

// SocketPath returns the socket path served inside dir.
func SocketPath(dir string) string { return filepath.Join(dir, SocketName) }

// Server is one per-child socket. Close stops the listener and the HTTP
// server and unlinks the socket file; it is safe to call more than once.
type Server struct {
	path string
	srv  *http.Server

	closeOnce sync.Once
	closeErr  error
}

// Close stops serving and unlinks the socket file. Safe to call repeatedly;
// the first call's outcome is remembered.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// Close the server first: it unblocks Serve, which releases the
		// listener. Closing the listener alone would leave in-flight
		// requests running.
		_ = s.srv.Close()
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// Serve listens on <dir>/connect.sock and reverse-proxies every request to
// target — the daemon's proxy-face URL, whose Connect route sits behind the
// same token middleware the children's own LLM traffic is authenticated
// through — with secret injected as the Authorization bearer credential.
//
// Inbound Authorization and X-Rafiki-* headers are stripped before
// forwarding: a caller that could smuggle its own credential past the
// injection would choose a stronger identity than the socket grants — the
// exact fail-open this proxy exists to make impossible. The request path is
// preserved, so a client speaks the same procedure URLs it would against the
// daemon directly.
//
// The listener serves HTTP/1.1 AND unencrypted HTTP/2 (h2c): a plain
// HTTP/1.1 client (httpx, curl --unix-socket) and a Connect h2c client both
// work, and Connect's server-streaming (Receive) rides either.
//
// dir is created 0700 and the socket is chmod'ed 0600 immediately after the
// bind — the directory is the access boundary (no other local user can
// traverse it), and the socket's own 0600 is defense in depth. There is
// deliberately NO umask dance: syscall.Umask is process-global, and a
// per-spawn dance would race every concurrent file creation the daemon
// makes.
//
// Stale socket files from a previous incarnation are removed before the
// bind, like serveConnectUDS does; a live server on that path is refused
// rather than clobbered.
func Serve(ctx context.Context, dir string, target *url.URL, secret string) (*Server, error) {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			stripCredentials(out.Header)
			out.Header.Set("Authorization", "Bearer "+secret)
			// Host and URL must name the target so the face's middleware and
			// ServeMux see the same request a direct client would send. The
			// request path is kept: it names the Connect procedure.
			out.Host = target.Host
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
		},
		// The Receive stream delivers a message roughly once per poll
		// interval; without immediate flushing a buffered proxy would hold
		// those bytes until the stream ends, and a waiting script would see
		// nothing until it stopped caring.
		FlushInterval: -1,
	}
	return serve(ctx, dir, proxy)
}

// ServeHandler is Serve against an http.Handler instead of a URL — the shape
// a test uses when it already holds the handler to proxy to. Header stripping
// and secret injection are identical: the handler receives exactly the
// request the URL form would have delivered.
func ServeHandler(ctx context.Context, dir string, h http.Handler, secret string) (*Server, error) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripCredentials(r.Header)
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Host = "connect.rafiki.invalid"
		h.ServeHTTP(w, r)
	})
	return serve(ctx, dir, handler)
}

func serve(ctx context.Context, dir string, handler http.Handler) (*Server, error) {
	sockPath := SocketPath(dir)

	// Refuse rather than clobber, exactly like serveConnectUDS: two servers on
	// one path means the second bind silently wins and the first's clients
	// connect into a void.
	if c, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is already served by a live listener", sockPath)
	}
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot remove stale socket %s: %w", sockPath, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	// The socket file's own mode is bound to the process umask, which this
	// call must not touch: syscall.Umask is process-global, and a dance here
	// (set 0177, bind, restore) racing any concurrent file creation would
	// clamp THAT creation to 0600 — and a botched restore would clamp every
	// later one. serveConnectUDS gets away with the dance because it runs
	// once at boot. Here, the 0700 directory is the real access boundary —
	// no other local user can traverse it to reach the socket — so the
	// chmod immediately after the bind is belt, not armor, and the race
	// window between bind and chmod is closed by the directory, not the
	// socket's own bits.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod %s: %w", sockPath, err)
	}

	// h2c + HTTP/1.1, per the package doc. A non-nil Protocols lists ONLY the
	// supported protocols, so HTTP/1 must be set explicitly alongside h2c —
	// a plain-httpx/curl client speaks HTTP/1.1, a Connect client speaks
	// prior-knowledge h2c, and both must work on one listener.
	proto := &http.Protocols{}
	proto.SetUnencryptedHTTP2(true)
	proto.SetHTTP1(true)
	srv := &http.Server{
		Handler:           handler,
		Protocols:         proto,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		_ = srv.Serve(ln)
	}()

	return &Server{path: sockPath, srv: srv}, nil
}

// stripCredentials removes every header a caller could use to present an
// identity of its own choosing. Authorization is the obvious one; the
// X-Rafiki-* family carries the daemon's own session attribution
// (X-Rafiki-Session on the per-boot credential) and must never survive a hop
// through a per-child socket — the socket speaks with exactly one voice.
func stripCredentials(h http.Header) {
	for k := range h {
		kanon := http.CanonicalHeaderKey(k)
		if kanon == "Authorization" || strings.HasPrefix(kanon, "X-Rafiki-") {
			h.Del(k)
		}
	}
}
