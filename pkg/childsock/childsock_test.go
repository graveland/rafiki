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

	"github.com/multigres/testkit/assert"
)

// TestServeProxyInjectsSecretAndStripsCallerCredentials is the checkpoint
// behaviour the wave-3 reviewer owns: the proxy speaks with exactly one
// voice. Whatever the caller sends in its own credential headers must not
// reach the target; the injected secret must.
func TestServeProxyInjectsSecretAndStripsCallerCredentials(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	var seen http.Header
	var seenPath string
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	c.NoError(err, "parse target")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "child-secret-1")
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.path, func(req *http.Request) {
		// The caller's own smuggled credentials: both must die in transit.
		req.Header.Set("Authorization", "Bearer EVIL-TOKEN")
		req.Header.Set("X-Rafiki-Session", "session-xyz")
		req.Header.Set("X-Rafiki-Other", "zzz")
	}, "/rafiki.v1.ControlService/Spawn")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	c.Eq(http.StatusOK, resp.StatusCode, "proxy status = %d, body %s", resp.StatusCode, body)

	c.Eq("/rafiki.v1.ControlService/Spawn", seenPath, "proxied path =")
	c.Eq("Bearer child-secret-1", seen.Get("Authorization"), "Authorization at target")
	for _, k := range []string{"X-Rafiki-Session", "X-Rafiki-Other"} {
		c.Eq("", seen.Get(k), "caller-supplied %s leaked to the target", k)
	}
}

// TestServeHandlerFormStripsAndInjects pins the two target shapes on the same
// credential behaviour: the handler form strips and injects exactly as the
// URL form does.
func TestServeHandlerFormStripsAndInjects(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	var seen http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeHandler(ctx, tempSocketDir(t), mux, "handler-secret")
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.path, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer EVIL")
		req.Header.Set("X-Rafiki-Session", "s")
	}, "/probe")
	defer resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "handler-proxy status =")
	c.Eq("Bearer handler-secret", seen.Get("Authorization"), "Authorization at handler =")
	c.Eq("", seen.Get("X-Rafiki-Session"), "X-Rafiki-Session leaked to the handler")
}

// TestServePermissions pins the filesystem contract: dir 0700, socket 0600.
// The socket IS the credential — a group- or world-readable one would hand
// the child's authority to every local user. The dir is a NOT-YET-EXISTING
// subdirectory so Serve's own MkdirAll does the creating (tempSocketDir
// pre-creates its dir at 0700, which would make the assertion
// self-fulfilling — MkdirAll would never re-mode an existing dir) and the
// pin actually tests the creation path.
func TestServePermissions(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	c.NoError(err, "parse")

	dir := filepath.Join(tempSocketDir(t), "host")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, dir, target, "s")
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	fi, err := os.Stat(dir)
	c.NoError(err, "stat dir")
	c.Eq(0o700, fi.Mode().Perm(), "dir mode")
	fi, err = os.Stat(srv.path)
	c.NoError(err, "stat socket")
	c.Eq(0o600, fi.Mode().Perm(), "socket mode")
}

// TestServeRefusesALiveListener mirrors serveConnectUDS's refuse-not-clobber
// rule: a second Serve on a live path must fail, never bind over it.
func TestServeRefusesALiveListener(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	c.NoError(err, "parse")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := Serve(ctx, tempSocketDir(t), target, "s")
	c.NoError(err, "first serve")
	t.Cleanup(func() { _ = first.Close() })

	if _, err := Serve(ctx, filepath.Dir(first.path), target, "s"); err == nil {
		t.Fatal("second Serve on a live path succeeded; it must refuse")
	}
}

// TestCloseUnlinksAndStopsServing pins the child-exit lifecycle: after Close,
// the socket file is gone and a dial fails.
func TestCloseUnlinksAndStopsServing(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	c.NoError(err, "parse")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "s")
	c.NoError(err, "serve")
	c.NoError(srv.Close(), "close")
	if _, err := os.Stat(srv.path); !os.IsNotExist(err) {
		t.Fatalf("socket file survived Close: err=%v", err)
	}
	if _, err := net.DialTimeout("unix", srv.path, time.Second); err == nil {
		t.Fatal("dial succeeded after Close; the listener is still serving")
	}
	// Idempotent.
	c.NoError(srv.Close(), "second close")
}

// TestServeHTTP11AndH2C proves both transports the plan promises work
// against one listener: a plain HTTP/1.1 client (the httpx/curl shape) and a
// prior-knowledge h2c client (the Connect client shape).
func TestServeHTTP11AndH2C(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(inner.Close)
	target, err := url.Parse(inner.URL)
	c.NoError(err, "parse")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := Serve(ctx, tempSocketDir(t), target, "s")
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	// HTTP/1.1: no http2 transport, just a unix dial.
	resp := dial(t, srv.path, nil, "/any")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "ok") {
		t.Fatalf("HTTP/1.1 round trip failed: %d %s", resp.StatusCode, b)
	}

	// h2c: prior-knowledge HTTP/2 over cleartext — the same shape
	// cmd/rafiki's Connect client uses against the daemon's control socket.
	h2c := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", srv.path)
		},
	}}
	req, err := http.NewRequest(http.MethodGet, "http://childsock.invalid/any", nil)
	c.NoError(err, "build h2c request")
	resp2, err := h2c.Do(req)
	c.NoError(err, "h2c request")
	defer resp2.Body.Close()
	c.Eq(http.StatusOK, resp2.StatusCode, "h2c request failed")
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
	assert.NewAborting(t).NoError(err, "mkdirtemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func dial(t *testing.T, path string, mutate func(*http.Request), urlPath string) *http.Response {
	t.Helper()
	c := assert.NewAborting(t)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	req, err := http.NewRequest(http.MethodGet, "http://childsock.invalid"+urlPath, nil)
	c.NoError(err, "build request")
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := client.Do(req)
	c.NoError(err, "dial %s", path)
	return resp
}
