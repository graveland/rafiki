package daraja

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
)

// newTestServer starts a real h2c server over the given host and returns a
// client, plus the *Server so a test can observe ShutdownRequested.
//
// h2c because connect-go refuses bidi streaming below HTTP/2; the server
// enables unencrypted HTTP/2 via http.Server.Protocols rather than the
// deprecated h2c handler, matching pkg/executor's test servers.
func newTestServer(t *testing.T, h *Host) (darajapbconnect.DarajaServiceClient, *Server) {
	t.Helper()
	srv := NewServer(h)
	path, handler := srv.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { httpSrv.Close() })
	go func() {
		_ = httpSrv.Serve(ln)
	}()

	// A plain *http.Client speaks HTTP/1.1, and Relay is bidi — connect-go
	// refuses bidi below HTTP/2 — so the client needs a real http2.Transport.
	// h2c has no TLS, hence AllowHTTP and a DialTLSContext that dials plain TCP.
	hc := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	return darajapbconnect.NewDarajaServiceClient(hc, "http://"+ln.Addr().String()), srv
}

func TestServerHealthReportsTheProcess(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	c, _ := newTestServer(t, h)
	resp, err := c.Health(context.Background(), connect.NewRequest(&darajapb.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Msg.GetRunning() {
		t.Error("running = false for a live process")
	}
	if resp.Msg.GetPid() == 0 {
		t.Error("pid = 0 for a live process")
	}
}

func TestServerShutdownEndsTheProcessAndSignalsExit(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c, srv := newTestServer(t, h)

	if _, err := c.Shutdown(context.Background(), connect.NewRequest(&darajapb.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if h.Running() {
		t.Error("process still running after Shutdown")
	}

	// daraja exits with its child; the server publishes that intent so the CLI
	// can return rather than serving a host with nothing in it.
	select {
	case <-srv.ShutdownRequested():
	default:
		t.Error("ShutdownRequested is not closed after a peer called Shutdown")
	}
}

func TestServerRelayCarriesStdioBothWays(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/cat"})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	c, _ := newTestServer(t, h)

	stream := c.Relay(context.Background())
	if err := stream.Send(&darajapb.RelayRequest{Stdin: []byte("ping\n")}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) {
		resp, err := stream.Receive()
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		got.Write(resp.GetStdout())
		if strings.Contains(got.String(), "ping") {
			return
		}
	}
	t.Fatalf("timeout; got %q", got.String())
}
