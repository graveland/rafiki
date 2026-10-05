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
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(err, "listen")
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
	ck := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	ck.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	c, _ := newTestServer(t, h)
	resp, err := c.Health(context.Background(), connect.NewRequest(&darajapb.HealthRequest{}))
	ck.Require().NoError(err, "Health")
	ck.True(resp.Msg.GetRunning(), "running = false for a live process")
	ck.NotEq(0, resp.Msg.GetPid(), "pid = 0 for a live process")
}

func TestServerShutdownEndsTheProcessAndSignalsExit(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	ck.Require().NoError(h.Start(), "Start")
	c, srv := newTestServer(t, h)

	_, err := c.Shutdown(context.Background(), connect.NewRequest(&darajapb.ShutdownRequest{}))
	ck.Require().NoError(err, "Shutdown")
	ck.False(h.Running(), "process still running after Shutdown")

	// daraja exits with its child; the server publishes that intent so the CLI
	// can return rather than serving a host with nothing in it.
	select {
	case <-srv.ShutdownRequested():
	default:
		t.Error("ShutdownRequested is not closed after a peer called Shutdown")
	}
}

// An event pulled off the host's channel and not delivered must be redelivered
// on the next stream. Without this, a reconnecting controller silently loses
// whatever was in flight when its connection broke — and nothing errors.
func TestUndeliveredEventSurvivesAFailedSend(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: "/bin/cat", Spec: ChildSpec{Kind: KindClaude}})
	s := NewServer(h)

	s.stash(&darajapb.RelayResponse{
		Event: &darajapb.RelayResponse_Stdout{Stdout: []byte("in flight")},
	})

	got := s.takePending()
	c.Require().NotNil(got, "stashed event was not returned")
	c.Eq("in flight", string(got.GetStdout()), "got %q, want", got.GetStdout())
	c.Nil(s.takePending(), "pending event was returned twice")
}

// Two Relay streams would split one event channel between them, giving each
// consumer a random half of the child's output.
func TestSecondRelayIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: "/bin/cat", Spec: ChildSpec{Kind: KindClaude}})
	s := NewServer(h)

	c.Require().True(s.attach(), "first attach was refused")
	c.False(s.attach(), "second attach was admitted; one Relay at a time is the contract")
	s.detach()
	c.True(s.attach(), "attach was refused after detach")
}

func TestServerRelayCarriesStdioBothWays(t *testing.T) {
	ck := assert.NewAborting(t)
	h := NewHost(HostOptions{Binary: testChildBinary(t, "cat"), Spec: ChildSpec{Kind: KindClaude}})
	ck.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	c, _ := newTestServer(t, h)

	stream := c.Relay(context.Background())
	ck.NoError(stream.Send(&darajapb.RelayRequest{Stdin: []byte("ping\n")}), "Send")

	deadline := time.Now().Add(5 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) {
		resp, err := stream.Receive()
		ck.NoError(err, "stream ended")
		got.Write(resp.GetStdout())
		if strings.Contains(got.String(), "ping") {
			return
		}
	}
	t.Fatalf("timeout; got %q", got.String())
}

// A Duration grace round-trips: Shutdown still ends the process when the
// caller sends an explicit span rather than an int count of milliseconds.
func TestServerShutdownAcceptsADurationGrace(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	ck.Require().NoError(h.Start(), "Start")
	c, _ := newTestServer(t, h)

	_, err := c.Shutdown(context.Background(), connect.NewRequest(&darajapb.ShutdownRequest{
		Grace: durationpb.New(200 * time.Millisecond),
	}))
	ck.Require().NoError(err, "Shutdown with a Duration grace")
	ck.False(h.Running(), "process still running after Shutdown")
}

// An out-of-range Duration is refused InvalidArgument before the host is
// touched, per design rule 4.
func TestServerShutdownRejectsOutOfRangeGrace(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	c, _ := newTestServer(t, h)

	_, err := c.Shutdown(context.Background(), connect.NewRequest(&darajapb.ShutdownRequest{
		Grace: &durationpb.Duration{Seconds: 1 << 62},
	}))
	ck.Require().NotNil(err, "an out-of-range grace must be refused")
	ck.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for an out-of-range grace")
}

// Restart refuses an out-of-range Duration the same way Shutdown does.
func TestServerRestartRejectsOutOfRangeGrace(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	c, _ := newTestServer(t, h)

	_, err := c.Restart(context.Background(), connect.NewRequest(&darajapb.RestartRequest{
		Grace: &durationpb.Duration{Seconds: 1 << 62},
	}))
	ck.Require().NotNil(err, "an out-of-range grace must be refused")
	ck.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for an out-of-range grace")
}
