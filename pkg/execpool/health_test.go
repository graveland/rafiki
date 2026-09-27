package execpool

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// blackHoleHandler answers Describe so the executor is admitted, then never
// answers Health. This is the shape of a slept laptop or a dropped NAT
// mapping: the socket is open, the peer is gone, and nothing at the TCP layer
// says so for another fifteen minutes.
type blackHoleHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	executorID string
}

func (h *blackHoleHandler) Describe(
	_ context.Context, _ *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId: h.executorID,
		Tools:      []string{"read", "bash"},
	}), nil
}

func (h *blackHoleHandler) Health(
	ctx context.Context, _ *connect.Request[executorpb.HealthRequest],
) (*connect.Response[executorpb.HealthResponse], error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The end-to-end case departure.go exists for and which had never been
// exercised: an executor that stops answering must be PARKED, on a timescale
// where the park window is still useful. With an unbounded Health the poll
// blocks on the first tick forever — the executor is never parked, its
// children are never told, and the goroutine leaks for the daemon's lifetime.
func TestUnresponsiveExecutorIsParkedRatherThanHangingForever(t *testing.T) {
	store := newFakeStore("exec-blackhole")
	p := New(store)
	p.healthInterval = 50 * time.Millisecond
	p.healthTimeout = 150 * time.Millisecond

	joinViaUpgrade(t, p, &blackHoleHandler{executorID: "exec-blackhole"})

	waitFor(t, 5*time.Second, "executor to join", func() bool {
		return len(p.Live()) == 1
	})
	waitFor(t, 5*time.Second, "unresponsive executor to be parked", func() bool {
		return p.Parked("exec-blackhole")
	})

	if _, err := p.ClientFor("exec-blackhole"); !errors.Is(err, ErrParked) {
		t.Fatalf("a parked executor must report ErrParked so children wait rather than fail: %v", err)
	}
}

// The join path in isolation. A peer that completes the upgrade handshake and
// then never speaks HTTP/2 must not hold the accept goroutine open
// indefinitely: Describe on the join path is bounded by joinTimeout.
func TestJoinDescribeIsBoundedByATimeout(t *testing.T) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	p := New(newFakeStore("exec-silent"))
	p.joinTimeout = 200 * time.Millisecond

	serveDone := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor),
		upgradeconn.Handler(upgradeconn.Executor, p.authorize,
			func(c *upgradeconn.Conn, a admission) {
				defer close(serveDone)
				p.serve(c, a)
			}))
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}
	// Upgrade and then go completely silent: never serve HTTP/2, never close.
	if _, _, err := upgradeconn.Dial(dialed, upgradeconn.Executor, "localhost",
		http.Header{"Authorization": {string(upgradeconn.SchemeBearer) + " c"}}); err != nil {
		t.Fatalf("upgrade refused: %v", err)
	}

	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serve never returned: Describe on the join path is unbounded")
	}
	if len(p.Live()) != 0 {
		t.Fatal("an executor that never answered Describe must not be admitted")
	}
}

// Keepalive is the half of A2 that no request-level timeout can cover: it is
// what makes a black-holed connection fail rather than sit there between
// polls. Asserted structurally because the alternative is a test that waits
// out a real TCP retransmission window.
func TestTransportEnablesHTTP2Keepalive(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	client, err := ClientForConn(c1)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := client.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http2.Transport", client.Transport)
	}
	if tr.ReadIdleTimeout <= 0 {
		t.Error("ReadIdleTimeout unset: a black-holed connection is only detected " +
			"when TCP retransmission gives up, roughly fifteen minutes later")
	}
	if tr.PingTimeout <= 0 {
		t.Error("PingTimeout unset: a PING that is never answered never fails the connection")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// joinViaUpgrade performs the full executor link against p: it dials in over
// TLS, upgrades with a Bearer credential through the pool's own
// authorization, and serves handler on the inverted connection — the same
// shape the raw hello-frame helper used to drive, now through the real
// upgrade path.
func joinViaUpgrade(t *testing.T, p *Pool, handler executorpbconnect.ExecutorServiceHandler) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), p.UpgradeHandler())
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close() })
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}

	upConn, _, err := upgradeconn.Dial(dialed, upgradeconn.Executor, "localhost",
		http.Header{"Authorization": {string(upgradeconn.SchemeBearer) + " c"}})
	if err != nil {
		return err // refused: the caller asserts on the refusal
	}

	_, h := executorpbconnect.NewExecutorServiceHandler(handler)
	go func() { _ = ServeInverted(t.Context(), upConn, h) }()
	return nil
}

func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}
