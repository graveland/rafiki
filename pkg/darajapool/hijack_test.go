// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
	reg := NewRegistry()
	tpk, err := reg.MintTicket("c1")
	c.NoError(err, "mint ticket")
	pool := New(reg)
	addr := servePoolOnTCP(t, pool)

	// The upgrade request itself carries the ticket; the fresh reconnect
	// credential rides back on the 101.
	upConn, resp, err := dialUpgrade(t, addr, ticketHeader("c1", tpk))
	c.NoError(err, "upgrade dial")
	c.NotEq("", resp.Get(upgradeconn.HeaderCredential), "no credential on the 101 response")

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
	c.NoError(err, "watch")
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
	// its upgrade, and is what starts stubDaraja.Relay sending.
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
	c.Len(got, stubMessageCount, "got %d stdout events, want %d (corrupted/dropped traffic over the hijacked connection)", len(got), stubMessageCount)
	for i, b := range got {
		want := stubLine(i)
		if !bytes.Equal(b, want) {
			t.Fatalf("event %d corrupted: got %d bytes, want %d bytes matching stubLine(%d)", i, len(b), len(want), i)
		}
	}

	upConn.Close()
	<-h2done
}
