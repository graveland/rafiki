package execpool

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
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

	if !pool.HasUDSEnrolled() {
		t.Error("HasUDSEnrolled should be true when a UDS executor exists")
	}
	if !pool.IsEnrolledViaUDS("e-uds") {
		t.Errorf("IsEnrolledViaUDS(e-uds) = false, want true")
	}
	if pool.IsEnrolledViaUDS("e-tcp") {
		t.Errorf("IsEnrolledViaUDS(e-tcp) = true, want false")
	}
	if pool.IsEnrolledViaUDS("nonexistent") {
		t.Errorf("IsEnrolledViaUDS(nonexistent) = true, want false")
	}

	// Remove UDS — only TCP remains.
	pool.removeLive("e-uds", lcUDP)
	if pool.HasUDSEnrolled() {
		t.Error("HasUDSEnrolled should be false after removing the only UDS executor")
	}
	if pool.IsEnrolledViaUDS("e-uds") {
		t.Errorf("IsEnrolledViaUDS(e-uds) = true after removal, want false")
	}

	// Remove TCP — empty pool.
	pool.removeLive("e-tcp", lcTCP)
	if pool.HasUDSEnrolled() {
		t.Error("HasUDSEnrolled should be false with no executors")
	}
}

// TestPoolNoExecutors verifies both methods return false when no executors are live.
func TestPoolNoExecutors(t *testing.T) {
	store := newFakeStore("e1")
	pool := New(store)

	if pool.HasUDSEnrolled() {
		t.Error("HasUDSEnrolled should be false when no executors are live")
	}
	if pool.IsEnrolledViaUDS("any") {
		t.Error("IsEnrolledViaUDS should be false for any id when none are live")
	}
}
