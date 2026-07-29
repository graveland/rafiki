package server

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// TestDeliverDoesNotBlockForeverOnUnreadSocket verifies that a subscriber
// that stops reading cannot block Deliver forever: the monitor goroutine
// that calls Deliver also drives status transitions and child-exit handling
// for that child, so an unbounded write would stall all bookkeeping.
//
// net.Pipe is fully synchronous and unbuffered (no internal buffering, no
// OS socket buffer), so a single unread Write already blocks immediately —
// there is no "fill the buffer first" step needed the way there would be
// with a real socket. One call is therefore enough to reproduce the
// condition; looping deliverWriteTimeout-bounded calls would only multiply
// the wall-clock cost (each bounded call still takes up to
// deliverWriteTimeout to return) without adding coverage.
func TestDeliverDoesNotBlockForeverOnUnreadSocket(t *testing.T) {
	client, server := net.Pipe() // never read from `client`
	t.Cleanup(func() { client.Close(); server.Close() })

	c := &netConn{conn: server}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Deliver(bytes.Repeat([]byte("x"), 64*1024))
	}()

	select {
	case <-done:
	case <-time.After(deliverWriteTimeout + 5*time.Second):
		t.Fatal("Deliver blocked on an unread socket — monitorChild would stall and the bus would drop terminal frames")
	}
}
