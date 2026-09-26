// SPDX-License-Identifier: Apache-2.0

package childsock

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestServeProxyInjectsSecretAndStripsCallerCredentials is the checkpoint
// behaviour the wave-3 reviewer owns: the proxy speaks with exactly one
// voice. Whatever the caller sends in its own credential headers must not
// reach the target; the injected secret must.
func TestServeProxyInjectsSecretAndStripsCallerCredentials(t *testing.T) {
	t.Parallel()

	var seen http.Header
	var seenPath string
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "child-secret-1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.path, func(req *http.Request) {
		// The caller's own smuggled credentials: both must die in transit.
		req.Header.Set("Authorization", "Bearer EVIL-TOKEN")
		req.Header.Set("X-Rafiki-Session", "session-xyz")
		req.Header.Set("X-Rafiki-Other", "zzz")
	}, "/rafiki.v1.ControlService/Spawn")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, body %s", resp.StatusCode, body)
	}

	if seenPath != "/rafiki.v1.ControlService/Spawn" {
		t.Fatalf("proxied path = %q", seenPath)
	}
	if got := seen.Get("Authorization"); got != "Bearer child-secret-1" {
		t.Fatalf("Authorization at target = %q, want the injected secret", got)
	}
	for _, k := range []string{"X-Rafiki-Session", "X-Rafiki-Other"} {
		if got := seen.Get(k); got != "" {
			t.Fatalf("caller-supplied %s leaked to the target: %q", k, got)
		}
	}
}

// TestServeHandlerFormStripsAndInjects pins the two target shapes on the same
// credential behaviour: the handler form strips and injects exactly as the
// URL form does.
func TestServeHandlerFormStripsAndInjects(t *testing.T) {
	t.Parallel()

	var seen http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeHandler(ctx, tempSocketDir(t), mux, "handler-secret")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.path, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer EVIL")
		req.Header.Set("X-Rafiki-Session", "s")
	}, "/probe")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handler-proxy status = %d", resp.StatusCode)
	}
	if got := seen.Get("Authorization"); got != "Bearer handler-secret" {
		t.Fatalf("Authorization at handler = %q", got)
	}
	if got := seen.Get("X-Rafiki-Session"); got != "" {
		t.Fatalf("X-Rafiki-Session leaked to the handler: %q", got)
	}
}

// TestServePermissions pins the filesystem contract: dir 0700, socket 0600.
// The socket IS the credential — a group- or world-readable one would hand
// the child's authority to every local user.
func TestServePermissions(t *testing.T) {
	t.Parallel()

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	dir := tempSocketDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, dir, target, "s")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
	fi, err = os.Stat(srv.path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600 (the socket is the credential)", got)
	}
}

// TestServeRefusesALiveListener mirrors serveConnectUDS's refuse-not-clobber
// rule: a second Serve on a live path must fail, never bind over it.
func TestServeRefusesALiveListener(t *testing.T) {
	t.Parallel()

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := Serve(ctx, tempSocketDir(t), target, "s")
	if err != nil {
		t.Fatalf("first serve: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := Serve(ctx, filepath.Dir(first.path), target, "s"); err == nil {
		t.Fatal("second Serve on a live path succeeded; it must refuse")
	}
}

// TestCloseUnlinksAndStopsServing pins the child-exit lifecycle: after Close,
// the socket file is gone and a dial fails.
func TestCloseUnlinksAndStopsServing(t *testing.T) {
	t.Parallel()

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "s")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(srv.path); !os.IsNotExist(err) {
		t.Fatalf("socket file survived Close: err=%v", err)
	}
	if _, err := net.DialTimeout("unix", srv.path, time.Second); err == nil {
		t.Fatal("dial succeeded after Close; the listener is still serving")
	}
	// Idempotent.
	if err := srv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestServeHTTP11AndH2C proves both transports the plan promises work
// against one listener: a plain HTTP/1.1 client (the httpx/curl shape) and a
// prior-knowledge h2c client (the Connect client shape).
func TestServeHTTP11AndH2C(t *testing.T) {
	t.Parallel()

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "s")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// HTTP/1.1: no http2 transport, just a unix dial.
	resp := dial(t, srv.path, nil, "/any")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "ok") {
		t.Fatalf("HTTP/1.1 round trip failed: %d %s", resp.StatusCode, b)
	}

	// h2c: prior-knowledge HTTP/2 over cleartext — the same shape
	// cmd/rafiki's Connect client uses against connect.sock.
	h2c := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", srv.path)
		},
	}}
	req, err := http.NewRequest(http.MethodGet, "http://childsock.invalid/any", nil)
	if err != nil {
		t.Fatalf("build h2c request: %v", err)
	}
	resp2, err := h2c.Do(req)
	if err != nil {
		t.Fatalf("h2c request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("h2c request failed: %d", resp2.StatusCode)
	}
}

// tempSocketDir is a socket path short enough for the kernel's 104-byte
// sun_path limit: t.TempDir() on darwin resolves through /private/var/folders
// and overruns it, so this pins the base at /tmp on darwin — the same
// discipline the integration harness applies.
func tempSocketDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "childsock-it-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func dial(t *testing.T, path string, mutate func(*http.Request), urlPath string) *http.Response {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	req, err := http.NewRequest(http.MethodGet, "http://childsock.invalid"+urlPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return resp
}
