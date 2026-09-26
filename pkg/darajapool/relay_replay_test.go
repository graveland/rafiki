// SPDX-License-Identifier: Apache-2.0

package darajapool

// The replay belt: events broadcast while the holder has no subscriber used
// to be dropped by the fan (select-default on a zero-subscriber set), and a
// holder-owned buffer died with the holder — which is how a fast script
// could lose its Exited event before the daemon-side pump's first Watch
// subscribe (or through daraja exiting with the script before anyone
// subscribed) and strand the child row as `streaming` forever. Wave 4's
// flagged residual, pinned here at three levels: the pool belt directly, the
// bound, and the real pool harness with a Runner subscribing after the
// script's whole life is already over.

import (
	"context"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"
)

// replayStdout reads a fanEvent into its stdout payload for assertions.
func replayStdout(ev *fanEvent) string { return string(ev.Response().GetStdout()) }

// TestPoolReplayBeltBuffersBeforeTheFirstSubscribe is the defect itself:
// emit a full script lifecycle with ZERO subscribers attached, then
// subscribe. The old fan dropped all three events silently; the belt must
// replay them in broadcast order. Fails against the pre-belt code.
func TestPoolReplayBeltBuffersBeforeTheFirstSubscribe(t *testing.T) {
	pool := New(NewRegistry())
	holder := newRelayHolder("c1", nil, pool)

	holder.broadcast(fanEvent{resp: stdout([]byte("first "))})
	holder.broadcast(fanEvent{resp: stdout([]byte("second "))})
	holder.broadcast(fanEvent{resp: exited(3, "")})

	ch, unsub := holder.subscribe()
	defer unsub()

	want := []struct {
		text string
		code int32
	}{
		{text: "first "},
		{text: "second "},
		{code: 3},
	}
	for i, w := range want {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event %d: channel closed early", i)
			}
			if ev.Err() != nil {
				t.Fatalf("event %d: unexpected stream error: %v", i, ev.Err())
			}
			if w.code == 0 {
				if got := replayStdout(ev); got != w.text {
					t.Fatalf("event %d: stdout = %q, want %q (replay dropped or reordered)", i, got, w.text)
				}
			} else {
				if ev.Response().GetExited() == nil {
					t.Fatalf("event %d: want an Exited event, got %T", i, ev.Response().GetEvent())
				}
				if code := ev.Response().GetExited().ExitCode; code != w.code {
					t.Fatalf("event %d: exit code = %d, want %d (the lost-exit defect)", i, code, w.code)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d never replayed: the pre-subscribe broadcast was dropped", i)
		}
	}
}

// TestPoolReplayBeltIsBoundedDropOldest pins the overflow policy: more than
// relayReplayMax pre-subscribe events keep the NEWEST relayReplayMax, in
// order — a terminal event is by definition the last one, so it must survive
// an overflow (the Exited broadcast itself evicts the oldest buffered event).
func TestPoolReplayBeltIsBoundedDropOldest(t *testing.T) {
	pool := New(NewRegistry())
	holder := newRelayHolder("c1", nil, pool)

	const emit = relayReplayMax + 50
	for i := range emit {
		holder.broadcast(fanEvent{resp: stdout([]byte{byte(i)})})
	}
	holder.broadcast(fanEvent{resp: exited(9, "")})

	if len(pool.replay["c1"]) != relayReplayMax {
		t.Fatalf("belt holds %d events, want the %d cap", len(pool.replay["c1"]), relayReplayMax)
	}

	ch, unsub := holder.subscribe()
	defer unsub()

	// The terminal event plus the newest relayReplayMax-1 stdout events must
	// arrive: every broadcast over the bound evicts the OLDEST buffered one,
	// and the Exited broadcast itself evicts one more (a terminal event is by
	// definition the last, so this is the policy holding).
	for i := range relayReplayMax - 1 {
		select {
		case ev := <-ch:
			b := ev.Response().GetStdout()
			if b == nil {
				t.Fatalf("event %d: want stdout, got %T", i, ev.Response().GetEvent())
			}
			if want := byte(i + 1 + (emit - relayReplayMax)); b[0] != want {
				t.Fatalf("event %d: seq %d, want %d (oldest events should have been dropped)", i, b[0], want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d never replayed", i)
		}
	}
	select {
	case ev := <-ch:
		if ev.Response().GetExited() == nil || ev.Response().GetExited().ExitCode != 9 {
			t.Fatalf("the terminal Exited did not survive the overflow: %T", ev.Response().GetEvent())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal Exited never replayed")
	}
}

// TestPoolReplayBeltSecondSubscriberGetsNoReplay pins the one-shot drain: the
// belt goes to the FIRST subscribe only. A later subscriber gets live events
// only — which is what makes replay safe for claude (no frame can be
// delivered twice to a consumer that already processed it, see the belt's
// doc comment in relay.go).
func TestPoolReplayBeltSecondSubscriberGetsNoReplay(t *testing.T) {
	pool := New(NewRegistry())
	holder := newRelayHolder("c1", nil, pool)

	holder.broadcast(fanEvent{resp: stdout([]byte("early"))})
	first, unsub1 := holder.subscribe()
	defer unsub1()

	// Drain the replay so the first subscriber is caught up.
	select {
	case ev := <-first:
		if replayStdout(ev) != "early" {
			t.Fatalf("first subscriber replay = %q, want %q", replayStdout(ev), "early")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first subscriber never received the replay")
	}

	second, unsub2 := holder.subscribe()
	defer unsub2()

	// Nothing may reach the second subscriber from the belt.
	select {
	case ev := <-second:
		t.Fatalf("the second subscriber received a replayed event: %+v", ev.Response())
	case <-time.After(100 * time.Millisecond):
	}

	// Live events flow to both.
	holder.broadcast(fanEvent{resp: stdout([]byte("live"))})
	for name, ch := range map[string]<-chan *fanEvent{"first": first, "second": second} {
		select {
		case ev := <-ch:
			if replayStdout(ev) != "live" {
				t.Fatalf("%s subscriber got %q, want %q", name, replayStdout(ev), "live")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s subscriber did not receive the live event", name)
		}
	}
}

// TestPoolReplayBeltSurvivesHolderDeath is the wave-4 residual's second
// half: daraja exits with a fast script, the holder is torn down with the
// connection BEFORE anyone subscribed — a holder-owned buffer would die with
// it. The belt must survive, and a Watch with no live connection must hand
// the events over (channel closes after) so the pump can still learn the
// outcome instead of retrying into a gone connection.
func TestPoolReplayBeltSurvivesHolderDeath(t *testing.T) {
	pool := New(NewRegistry())
	holder := newRelayHolder("c1", nil, pool)

	holder.broadcast(fanEvent{resp: stdout([]byte("life "))})
	holder.broadcast(fanEvent{resp: exited(7, "")})

	// The connection dies; the holder is torn down — nothing subscribed yet.
	holder.stop()

	ch, unsub, err := pool.Watch("c1")
	if err != nil {
		t.Fatalf("Watch with a non-empty belt must serve the belt, got: %v", err)
	}
	defer unsub()

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("belt channel closed before delivering the events")
		}
		if replayStdout(ev) != "life " {
			t.Fatalf("belt delivered %q, want the buffered stdout", replayStdout(ev))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the belt was lost with the holder (the pre-fix strand)")
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("belt channel closed before delivering the exit")
		}
		if ev.Response().GetExited() == nil || ev.Response().GetExited().ExitCode != 7 {
			t.Fatalf("belt delivered %T, want the Exited event", ev.Response().GetEvent())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the exit never survived the holder death")
	}
	// Delivered exactly once: the channel closes after the belt.
	if _, ok := <-ch; ok {
		t.Fatal("the belt channel must close after handing the events over")
	}

	// And a Watch afterwards propagates the no-connection error (the pump's
	// retry contract) — the belt was consumed.
	_, _, err = pool.Watch("c1")
	if err == nil {
		t.Fatal("Watch with no connection and an empty belt must fail (retry contract)")
	}
}

// stubFastExitDaraja answers Relay by emitting a script's whole life — one
// stdout chunk, then the exit — the instant the stream opens, and then
// RETURNS, ending the stream the way a script's daraja does when the child
// is done. The daemon-side holder therefore dies before any subscriber can
// exist: this is the production drop window end to end.
type stubFastExitDaraja struct{}

func (stubFastExitDaraja) Relay(ctx context.Context, stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse]) error {
	if err := stream.Send(&darajapb.RelayResponse{
		Event: &darajapb.RelayResponse_Stdout{Stdout: []byte("fast-out\n")},
	}); err != nil {
		return err
	}
	if err := stream.Send(&darajapb.RelayResponse{
		Event: &darajapb.RelayResponse_Exited{Exited: &darajapb.ProcessExited{ExitCode: 7}},
	}); err != nil {
		return err
	}
	// Returning ends the relay stream: the pool's recvLoop sees the stream
	// end, tears the holder down, and the connection lifecycle completes —
	// while the test's runner has not subscribed yet.
	return nil
}

func (stubFastExitDaraja) Restart(context.Context, *connect.Request[darajapb.RestartRequest]) (*connect.Response[darajapb.RestartResponse], error) {
	return connect.NewResponse(&darajapb.RestartResponse{}), nil
}

func (stubFastExitDaraja) Shutdown(context.Context, *connect.Request[darajapb.ShutdownRequest]) (*connect.Response[darajapb.ShutdownResponse], error) {
	return connect.NewResponse(&darajapb.ShutdownResponse{}), nil
}

func (stubFastExitDaraja) Health(context.Context, *connect.Request[darajapb.HealthRequest]) (*connect.Response[darajapb.HealthResponse], error) {
	return connect.NewResponse(&darajapb.HealthResponse{Running: true}), nil
}

// TestScriptExitBeforeTheFirstSubscribeStillReachesTheRunner is the chain
// witness for the flake: the script's stdout and Exited land in the pool's
// belt (asserted directly, not by timing) and the holder is then torn down
// with the connection — and the runner, subscribing only afterwards, must
// still deliver stdout AND the exit, the two inputs a script child's settle
// is computed from. With the pre-belt fan this hangs as "streaming forever".
func TestScriptExitBeforeTheFirstSubscribeStillReachesTheRunner(t *testing.T) {
	pool, _, teardown := connectFakeDaraja(t, stubFastExitDaraja{})
	defer teardown()

	// The harness returns once c1 is live; the holder exists by then. Wait
	// until the belt has actually BUFFERED the exit — proving the drop
	// window materialized rather than the events merely being in flight.
	bufferedExited := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.RLock()
		evts := pool.replay["c1"]
		for _, ev := range evts {
			if ev.Response().GetExited() != nil {
				bufferedExited = true
			}
		}
		pool.mu.RUnlock()
		if bufferedExited {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bufferedExited {
		t.Fatal("the exit was never buffered pre-subscribe — the test no longer reproduces the drop window")
	}

	r := NewRunner(pool, "c1")
	_, stdoutR, _, err := r.Start()
	if err != nil {
		t.Fatalf("runner start: %v", err)
	}
	defer stdoutR.Close()

	// The stdout pipe is SYNCHRONOUS (io.Pipe): the replayed stdout chunk
	// blocks the drain loop until a reader consumes it, so drain it on its
	// own goroutine the way pkg/child does.
	stdoutDone := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(stdoutR)
		stdoutDone <- string(b)
	}()

	done := make(chan [2]int, 1)
	go func() {
		code, sig := r.Wait()
		done <- [2]int{code, len(sig)}
	}()
	select {
	case got := <-done:
		if got[0] != 7 {
			t.Fatalf("Wait = code %d, want 7 (the lost-exit defect: the child would hang as streaming forever)", got[0])
		}
		if out := <-stdoutDone; out != "fast-out\n" {
			t.Fatalf("stdout = %q, want the replayed chunk verbatim", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the script's exit never reached the runner (the child would hang as streaming forever)")
	}
}
