package execpool

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// Regression pin: ServeInverted used to run http2.Server.ServeConn with a
// hardcoded context.Background(), so a caller's shutdown context being
// canceled had no effect on it at all — the connection-level frame loop
// never selected on that context (only per-request contexts derive from it;
// see http2's serverConnBaseContext), so ServeConn just blocked on the
// connection until something else closed it. In practice that meant a
// SIGTERM to `rafiki executor serve` did nothing while it was actively
// connected, and only SIGKILL ever actually ended it.
func TestServeInvertedStopsWhenContextIsCanceled(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- ServeInverted(ctx, server, http.NotFoundHandler())
	}()

	// Let ServeConn start its blocking read for the HTTP/2 preface, which the
	// client side never sends — this is what would hang forever pre-fix.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeInverted did not return within 2s of its context being canceled")
	}
}

// TestPoolTransportMethods verifies HasUDSEnrolled and IsEnrolledViaUDS
// operate correctly on the live map's network field.
func TestPoolTransportMethods(t *testing.T) {
	c := assert.NewCollecting(t)
	store := newFakeStore("e1")
	pool := New(store)

	lcTCP := &liveConn{
		executor:    store.executor,
		describe:    nil,
		connectedAt: time.Now(),
		network:     "tcp",
		done:        make(chan struct{}),
	}

	lcUDP := &liveConn{
		executor:    store.executor,
		describe:    nil,
		connectedAt: time.Now(),
		network:     "unix",
		done:        make(chan struct{}),
	}

	pool.installLive("e-tcp", lcTCP)
	pool.installLive("e-uds", lcUDP)

	c.True(pool.HasUDSEnrolled(), "HasUDSEnrolled should be true when a UDS executor exists")
	c.True(pool.IsEnrolledViaUDS("e-uds"), "IsEnrolledViaUDS(e-uds) = false, want true")
	c.False(pool.IsEnrolledViaUDS("e-tcp"), "IsEnrolledViaUDS(e-tcp) = true, want false")
	c.False(pool.IsEnrolledViaUDS("nonexistent"), "IsEnrolledViaUDS(nonexistent) = true, want false")

	// Remove UDS — only TCP remains.
	pool.removeLive("e-uds", lcUDP)
	c.False(pool.HasUDSEnrolled(), "HasUDSEnrolled should be false after removing the only UDS executor")
	c.False(pool.IsEnrolledViaUDS("e-uds"), "IsEnrolledViaUDS(e-uds) = true after removal, want false")

	// Remove TCP — empty pool.
	pool.removeLive("e-tcp", lcTCP)
	c.False(pool.HasUDSEnrolled(), "HasUDSEnrolled should be false with no executors")
}

// TestPoolNoExecutors verifies both methods return false when no executors are live.
func TestPoolNoExecutors(t *testing.T) {
	c := assert.NewCollecting(t)
	store := newFakeStore("e1")
	pool := New(store)

	c.False(pool.HasUDSEnrolled(), "HasUDSEnrolled should be false when no executors are live")
	c.False(pool.IsEnrolledViaUDS("any"), "IsEnrolledViaUDS should be false for any id when none are live")
}

// TestStreamLimitWaitsRatherThanFails pins the fix for call #251 on a single
// executor connection: there is ONE connection per executor and the dial hook
// refuses a second, so once the server's MaxConcurrentStreams is reached the
// transport must make an additional caller WAIT for a free stream rather than
// open a second connection. Without StrictMaxConcurrentStreams the transport
// takes the latter path, the hook answers ErrRedialed, and the call fails.
//
// The server is built with the same construction as production
// (newInvertedServer) but with a two-stream limit so the third concurrent call
// is the one that must wait.
func TestStreamLimitWaitsRatherThanFails(t *testing.T) {
	c := assert.NewCollecting(t)
	client, server := net.Pipe()
	defer client.Close()

	block := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/warm", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	})

	go func() { _ = serveInvertedOn(t.Context(), server, mux, newInvertedServer(2)) }()

	httpClient, err := ClientForConn(client)
	c.NoError(err, "ClientForConn")

	// Warm the connection first, so the client has processed the server's
	// SETTINGS frame (MaxConcurrentStreams: 2) before the three calls race for
	// streams.
	warm, err := httpClient.Get("http://executor/warm")
	c.NoError(err, "warmup request")
	warm.Body.Close()

	type result struct{ err error }
	results := make(chan result, 3)
	for i := 0; i < 3; i++ {
		go func() {
			resp, err := httpClient.Get("http://executor/")
			if err == nil {
				resp.Body.Close()
			}
			results <- result{err}
		}()
	}

	// Two calls hold the two stream slots; the third must be waiting for one to
	// free, not failing. If StrictMaxConcurrentStreams were removed the third
	// would already have failed with ErrRedialed by now.
	select {
	case r := <-results:
		t.Fatalf("a call returned before the handler was released: err=%v", r.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(block)

	for i := 0; i < 3; i++ {
		r := <-results
		c.NoError(r.err, "call after release")
	}
}
