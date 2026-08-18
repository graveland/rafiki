package execpool

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// The whole inversion, over TLS, with Connect on top — the combination the
// spike did NOT cover (it used raw net/http over http2).
func TestConnectOverInvertedTLSConnection(t *testing.T) {
	// rafikid: listens. executor: dials. TLS roles follow TCP; HTTP roles
	// invert on top.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		accepted <- c
	}()

	// Executor side: dial, then SERVE on what you dialled.
	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}
	// http/1.1, not h2 — and this is the point rather than a regression.
	//
	// ALPN used to be what made both sides agree to speak HTTP/2, and getting it
	// wrong was silent: omit NextProtos and it resolves to "" while ServeConn
	// works anyway. It is no longer load-bearing. The connection now starts as
	// an ordinary HTTP/1.1 request that is UPGRADED (net/http can only hijack
	// 1.1), and the inverted h2 below begins on the raw byte stream afterwards,
	// where ALPN is not consulted at all. Agreement is now the Upgrade header's
	// job, which fails with a readable HTTP status instead of a TLS alert.
	//
	// This test drives the inversion directly, without the upgrade in front, to
	// keep the transport primitive covered on its own.
	if got := dialed.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("ALPN negotiated %q, want http/1.1", got)
	}

	_, fakeHandler := executorpbconnect.NewExecutorServiceHandler(
		&stubHandler{executorID: "test-executor"})
	go func() {
		_ = ServeInverted(dialed, fakeHandler)
	}()

	srvConn := <-accepted
	httpClient, err := ClientForConn(srvConn)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // let the server start
	cl := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")

	// Unary.
	resp, err := cl.Describe(context.Background(),
		connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe over the inverted connection: %v", err)
	}
	if resp.Msg.ExecutorId != "test-executor" {
		t.Fatalf("empty Describe response: %+v", resp.Msg)
	}

	// Server-stream, and finding 4.
	stream, err := cl.Execute(context.Background(),
		connect.NewRequest(&executorpb.ExecuteRequest{Tool: "bash", CallId: "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("no first chunk: %v", stream.Err())
	}

	// Finding 2 (concurrency).
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cl.Health(context.Background(),
				connect.NewRequest(&executorpb.HealthRequest{})); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}
}

// Finding 2, isolated: the hook hands the connection over exactly once.
func TestSecondDialAttemptIsReportedNotSwallowed(t *testing.T) {
	// Simpler: use a plain server that closes, and check the error chain.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	acceptCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			errCh <- aErr
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		acceptCh <- c
	}()

	dialed, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	if err := dialed.Handshake(); err != nil {
		t.Fatal(err)
	}

	srvConn := <-acceptCh
	if tc, ok := srvConn.(*tls.Conn); ok {
		_ = tc.Handshake()
	}

	httpClient, err := ClientForConn(srvConn)
	if err != nil {
		t.Fatal(err)
	}

	_, handler := executorpbconnect.NewExecutorServiceHandler(
		&stubHandler{executorID: "redial-test"})
	go func() {
		_ = ServeInverted(dialed, handler)
	}()
	time.Sleep(10 * time.Millisecond)

	cl := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")
	_, err = cl.Health(context.Background(), connect.NewRequest(&executorpb.HealthRequest{}))
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Force the transport to give up on the connection by closing it.
	_ = srvConn.Close()

	// Second call must return an error wrapping ErrRedialed.
	_, err = cl.Health(context.Background(), connect.NewRequest(&executorpb.HealthRequest{}))
	if err == nil {
		t.Fatal("want error after connection close, got nil")
	}
	if !isRedialed(err) {
		t.Fatalf("want ErrRedialed, got %v", err)
	}
}

func isRedialed(err error) bool {
	for {
		if err == ErrRedialed {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			// Try errors.Unwrap (for %w chains)
			unwrapped := errorsUnwrap(err)
			if unwrapped == nil {
				return false
			}
			err = unwrapped
			continue
		}
		err = u.Unwrap()
	}
}

func errorsUnwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}
