package execpool

import (
	"testing"
	"time"
)

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
