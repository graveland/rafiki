// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// connectFakeDaraja drives a REAL hijacked HTTP/2 connection between a
// darajapool.Pool and handler (a darajapbconnect.DarajaServiceHandler),
// exactly the shape TestHandleConnDeliversUncorruptedTrafficOverARealHijack
// proved is necessary — net.Pipe has none of net/http's post-hijack
// connReader state machine, so a net.Pipe-based harness cannot exercise the
// concurrent-read/stream-lifetime bugs a real hijack can (see that test's
// doc comment; this is the SAME harness, factored out so other tests in this
// package can plug in a different stub handler instead of stubDaraja).
//
// Returns the pool, the childID the fake daraja connected as ("c1"), and a
// teardown func that must be called (directly or via t.Cleanup) exactly
// once.
func connectFakeDaraja(t *testing.T, handler darajapbconnect.DarajaServiceHandler) (pool *Pool, childID string, teardown func()) {
	t.Helper()

	reg := NewRegistry()
	tpk, err := reg.MintTicket("c1")
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	pool = New(reg)

	addr := servePoolOnTCP(t, pool)

	// Authenticate on the upgrade request itself: the one-shot ticket plus
	// the child id, exactly what a real daraja's first dial sends. The fresh
	// reconnect credential comes back as a header on the 101.
	upConn, resp, err := dialUpgrade(t, addr, ticketHeader("c1", tpk))
	if err != nil {
		t.Fatalf("upgrade dial: %v", err)
	}
	if resp.Get(upgradeconn.HeaderCredential) == "" {
		t.Fatal("no credential on the 101 response")
	}

	deadline := time.After(3 * time.Second)
	for {
		live := pool.Live()
		if len(live) == 1 && live[0] == "c1" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("c1 never appeared in Live(): %v", live)
		case <-time.After(10 * time.Millisecond):
		}
	}

	path, h := darajapbconnect.NewDarajaServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	h2s := &http2.Server{}
	h2done := make(chan struct{})
	go func() {
		defer close(h2done)
		h2s.ServeConn(upConn, &http2.ServeConnOpts{Handler: mux})
	}()

	teardown = func() {
		upConn.Close()
		<-h2done
	}
	return pool, "c1", teardown
}
