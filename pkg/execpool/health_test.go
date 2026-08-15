package execpool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
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

	srvConn := invertedPair(t, &blackHoleHandler{executorID: "exec-blackhole"})

	go p.handleConn(srvConn)

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

// The join path in isolation. The hello frame has a read deadline, but it is a
// CONNECTION deadline and has to be cleared before the connection goes on to
// live for hours — which left Describe as the one unbounded call on the
// admission path. A peer that completes the handshake and then never speaks
// HTTP/2 held an accept goroutine and its connection open indefinitely.
func TestJoinDescribeIsBoundedByATimeout(t *testing.T) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		accepted <- c
	}()

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}
	// Send a well-formed hello and then go completely silent: never serve
	// HTTP/2, never close.
	sendHelloFrame(t, dialed, protocol.ExecutorHelloRequest{Credential: "c"})

	p := New(newFakeStore("exec-silent"))
	p.joinTimeout = 200 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConn(<-accepted)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn never returned: Describe on the join path is unbounded")
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

// invertedPair stands up the real arrangement — executor dials and SERVES,
// rafikid accepts and is the client — and returns rafikid's side, already
// carrying a hello frame so handleConn can be driven directly.
func invertedPair(t *testing.T, handler executorpbconnect.ExecutorServiceHandler) net.Conn {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		accepted <- c
	}()

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close() })
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}

	sendHelloFrame(t, dialed, protocol.ExecutorHelloRequest{Credential: "c"})

	_, h := executorpbconnect.NewExecutorServiceHandler(handler)
	go func() {
		// Drain rafikid's hello RESPONSE before serving. Byte-at-a-time, so
		// it stops exactly at the newline and leaves the HTTP/2 preface for
		// the server — the same discipline readHelloFrame follows on the
		// other side, and for the same reason.
		if _, err := readHelloResponseLine(dialed); err != nil {
			return
		}
		_ = ServeInverted(dialed, h)
	}()

	select {
	case c := <-accepted:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
		return nil
	}
}

func sendHelloFrame(t *testing.T, w net.Conn, req protocol.ExecutorHelloRequest) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func readHelloResponseLine(r net.Conn) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for {
		if _, err := r.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(buf), nil
		}
		buf = append(buf, one[0])
	}
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
