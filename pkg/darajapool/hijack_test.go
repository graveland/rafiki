// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

const (
	stubMessageCount = 200
	stubPayloadSize  = 2 * 1024
)

// stubDaraja is a minimal DarajaServiceHandler that streams stubMessageCount
// stdout events on Relay, generating real HTTP/2 read traffic on the
// connection — the same kind of traffic recvLoop consumes in production.
type stubDaraja struct{}

func stubLine(i int) []byte {
	payload := bytes.Repeat([]byte{byte(i)}, stubPayloadSize)
	return append([]byte(fmt.Sprintf("line %04d ", i)), payload...)
}

func (stubDaraja) Relay(ctx context.Context, stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse]) error {
	for i := 0; i < stubMessageCount; i++ {
		if err := stream.Send(&darajapb.RelayResponse{
			Event: &darajapb.RelayResponse_Stdout{Stdout: stubLine(i)},
		}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (stubDaraja) Restart(context.Context, *connect.Request[darajapb.RestartRequest]) (*connect.Response[darajapb.RestartResponse], error) {
	return connect.NewResponse(&darajapb.RestartResponse{}), nil
}

func (stubDaraja) Shutdown(context.Context, *connect.Request[darajapb.ShutdownRequest]) (*connect.Response[darajapb.ShutdownResponse], error) {
	return connect.NewResponse(&darajapb.ShutdownResponse{}), nil
}

func (stubDaraja) Health(context.Context, *connect.Request[darajapb.HealthRequest]) (*connect.Response[darajapb.HealthResponse], error) {
	return connect.NewResponse(&darajapb.HealthResponse{Running: true}), nil
}

// TestHandleConnDeliversUncorruptedTrafficOverARealHijack drives real HTTP/2
// traffic through the daraja accept path over an ACTUAL http.Hijacker rather
// than net.Pipe. net.Pipe has none of net/http's post-hijack connReader state
// machine, so a net.Pipe-based test cannot exercise concurrent-read or
// stream-lifetime bugs on the hijacked connection the way a real hijack can.
func TestHandleConnDeliversUncorruptedTrafficOverARealHijack(t *testing.T) {
	reg := NewRegistry()
	tpk, _ := reg.MintTicket("c1")
	pool := New(reg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: pool.UpgradeHandler()}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	upConn, err := upgradeconn.Dial(conn, upgradeconn.Daraja, ln.Addr().String())
	if err != nil {
		t.Fatalf("upgrade dial: %v", err)
	}

	hello := protocol.DarajaHelloRequest{Type: "daraja_hello", ChildID: "c1", Ticket: tpk}
	helloJSON, _ := json.Marshal(hello)
	helloJSON = append(helloJSON, '\n')
	if _, err := upConn.Write(helloJSON); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// Read the hello response line via the same reader upgradeconn hands
	// back, so the HTTP/2 server that follows starts at the right byte.
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

	// Wait for installLive; the relay holder is registered synchronously
	// right alongside it, before its startIn goroutine actually runs.
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

	// Subscribe BEFORE any traffic is produced, so nothing is missed to the
	// fan-out's drop-if-slow behaviour.
	events, unsub, err := pool.Watch("c1")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer unsub()

	received := make(chan [][]byte, 1)
	go func() {
		var got [][]byte
		for i := 0; i < stubMessageCount; i++ {
			select {
			case ev, ok := <-events:
				if !ok {
					received <- got
					return
				}
				if err := ev.Err(); err != nil {
					t.Errorf("relay stream error after %d events: %v", len(got), err)
					received <- got
					return
				}
				got = append(got, ev.Response().GetStdout())
			case <-time.After(5 * time.Second):
				received <- got
				return
			}
		}
		received <- got
	}()

	// Now play daraja's role: serve HTTP/2 on the connection the pool holds
	// as its client transport. This is exactly what real daraja does after
	// its hello exchange, and is what starts stubDaraja.Relay sending.
	path, handler := darajapbconnect.NewDarajaServiceHandler(stubDaraja{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	h2s := &http2.Server{}
	h2done := make(chan struct{})
	go func() {
		defer close(h2done)
		h2s.ServeConn(upConn, &http2.ServeConnOpts{Handler: mux})
	}()

	got := <-received
	if len(got) != stubMessageCount {
		t.Fatalf("got %d stdout events, want %d (corrupted/dropped traffic over the hijacked connection)", len(got), stubMessageCount)
	}
	for i, b := range got {
		want := stubLine(i)
		if !bytes.Equal(b, want) {
			t.Fatalf("event %d corrupted: got %d bytes, want %d bytes matching stubLine(%d)", i, len(b), len(want), i)
		}
	}

	conn.Close()
	<-h2done
}
