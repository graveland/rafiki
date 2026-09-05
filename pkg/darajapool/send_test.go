// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"
)

// recordingDaraja is a DarajaServiceHandler whose Relay reads every RelayRequest
// it receives and pushes its Stdin bytes onto received, so a test can assert
// on how many separate writes actually arrived and in what order.
type recordingDaraja struct {
	received chan []byte
}

func (d *recordingDaraja) Relay(ctx context.Context, stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse]) error {
	for {
		req, err := stream.Receive()
		if err != nil {
			return err
		}
		if req == nil {
			continue // the Send(nil) that opens the stream
		}
		d.received <- req.Stdin
	}
}

func (d *recordingDaraja) Restart(context.Context, *connect.Request[darajapb.RestartRequest]) (*connect.Response[darajapb.RestartResponse], error) {
	return connect.NewResponse(&darajapb.RestartResponse{}), nil
}

func (d *recordingDaraja) Shutdown(context.Context, *connect.Request[darajapb.ShutdownRequest]) (*connect.Response[darajapb.ShutdownResponse], error) {
	return connect.NewResponse(&darajapb.ShutdownResponse{}), nil
}

func (d *recordingDaraja) Health(context.Context, *connect.Request[darajapb.HealthRequest]) (*connect.Response[darajapb.HealthResponse], error) {
	return connect.NewResponse(&darajapb.HealthResponse{Running: true}), nil
}

// TestSendCanBeCalledMoreThanOnce is the regression test for the bug where
// Pool.Send called holder.closeWrite() after every write, half-closing the
// shared relay stream's request side and making a second Send fail. Real use
// (every turn after the first on a daraja-hosted claude child) needs many
// sends across the child's whole life.
func TestSendCanBeCalledMoreThanOnce(t *testing.T) {
	stub := &recordingDaraja{received: make(chan []byte, 8)}
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	if err := pool.Send(childID, []byte("first\n")); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := pool.Send(childID, []byte("second\n")); err != nil {
		t.Fatalf("second send: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var got [][]byte
	for len(got) < 2 {
		select {
		case b := <-stub.received:
			got = append(got, b)
		case <-deadline:
			t.Fatalf("only received %d of 2 sends: %v", len(got), got)
		}
	}

	if string(got[0]) != "first\n" || string(got[1]) != "second\n" {
		t.Fatalf("got %q, %q; want %q, %q", got[0], got[1], "first\n", "second\n")
	}
}
