package childsock

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeTransportUsesTheCallerRoundTripper pins the executor-hosted shape:
// the outbound side of the proxy is whatever transport the caller hands over.
// The executor passes a TLS-with-pinned-cert transport (its target is the
// daemon's control listener) or a unix dialer (the daemon's local Connect
// socket); this test proves the transport is USED — a transport that answers
// with its own marker must be the one the request reaches, whatever the
// target URL claims.
func TestServeTransportUsesTheCallerRoundTripper(t *testing.T) {
	seen := make(chan *http.Request, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		_, _ = w.Write([]byte("via-custom-rt"))
	}))
	t.Cleanup(backend.Close)

	// A RoundTripper that routes EVERYTHING to the backend, ignoring the
	// target URL entirely — the same posture the unix transport has (the URL's
	// host is a placeholder; the dialer decides where the bytes go).
	rt := rtToBackend{backend: backend.URL}

	// The target URL is deliberately unreachable on its own (nothing listens
	// there): if the proxy used the default transport instead of the caller's,
	// every request would 503 instead of arriving.
	target := &url.URL{Scheme: "https", Host: "no-such-host.test:1"}
	dir := tempSocketDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeTransport(ctx, dir, target, "child-secret", rt)
	if err != nil {
		t.Fatalf("ServeTransport: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, SocketPath(dir), nil, "/rafiki.v1.Control/GetPreset")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "via-custom-rt") {
		t.Fatalf("response %q did not come through the caller's transport", body)
	}
	select {
	case r := <-seen:
		if got := r.Header.Get("Authorization"); got != "Bearer child-secret" {
			t.Fatalf("Authorization = %q, want the injected child secret", got)
		}
	default:
		t.Fatal("the backend saw no request")
	}
}

// TestUnixTransportDialsTheSocket pins the executor-side unix face target: the
// transport dials the named unix socket and ignores the URL's host, which is
// the only way to reach a daemon whose Connect plane has no TCP address.
func TestUnixTransportDialsTheSocket(t *testing.T) {
	sock := filepath.Join(tempSocketDir(t), "face.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan *http.Request, 4)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		_, _ = w.Write([]byte("over-unix"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cli := &http.Client{Transport: UnixTransport(sock), Timeout: 5 * time.Second}
	resp, err := cli.Get("http://connect.rafiki.invalid/anything")
	if err != nil {
		t.Fatalf("through UnixTransport: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "over-unix" {
		t.Fatalf("body = %q, want the unix face's answer", body)
	}
	select {
	case <-got:
	default:
		t.Fatal("the unix face saw no request")
	}
}

// A daemon that is down (or restarting) is not a script bug: the proxy must
// answer a typed Connect error — HTTP 503 carrying code "unavailable" — so
// the SDK's bounded-backoff retry can recognise it, and the body must never
// carry credentials (it carries only the transport failure).
func TestProxyErrorSurfacesUnavailable(t *testing.T) {
	// A transport that always fails, like a dial into a dead daemon.
	rt := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: errDaemonDown{}}
		},
	}
	target := &url.URL{Scheme: "https", Host: "no-such-host.test:1"}
	dir := tempSocketDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeTransport(ctx, dir, target, "secret", rt)
	if err != nil {
		t.Fatalf("ServeTransport: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, SocketPath(dir), nil, "/rafiki.v1.Control/GetPreset")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 ServiceUnavailable (a Connect client maps it to Unavailable)", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"unavailable"`) {
		t.Fatalf("body %q does not name code unavailable", body)
	}
	if strings.Contains(string(body), "secret") {
		t.Fatalf("the error body carries the child secret: %q", body)
	}
}

type errDaemonDown struct{}

func (errDaemonDown) Error() string { return "daemon down" }

// rtToBackend is a full RoundTripper that rewrites every request onto its
// backend as plain HTTP, whatever the request's original scheme/host said.
type rtToBackend struct{ backend string }

func (r rtToBackend) RoundTrip(req *http.Request) (*http.Response, error) {
	bu, err := url.Parse(r.backend)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = bu.Scheme
	u.Host = bu.Host
	clone.URL = &u
	return http.DefaultTransport.RoundTrip(clone)
}
