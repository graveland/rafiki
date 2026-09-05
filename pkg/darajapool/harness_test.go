// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: pool.UpgradeHandler()}
	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	upConn, err := upgradeconn.Dial(conn, upgradeconn.Daraja, ln.Addr().String())
	if err != nil {
		t.Fatalf("upgrade dial: %v", err)
	}

	hello := protocol.DarajaHelloRequest{Type: "daraja_hello", ChildID: "c1", Ticket: tpk}
	helloJSON, err := json.Marshal(hello)
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	helloJSON = append(helloJSON, '\n')
	if _, err := upConn.Write(helloJSON); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	line, err := upConn.Reader().ReadString('\n')
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	var resp protocol.DarajaHelloResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("parse hello response %q: %v", line, err)
	}
	if resp.Error != "" {
		t.Fatalf("hello refused: %s", resp.Error)
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
		conn.Close()
		<-h2done
		srv.Close()
		ln.Close()
	}
	return pool, "c1", teardown
}
