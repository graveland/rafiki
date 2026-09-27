// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"

	"github.com/multigres/testkit/assert"
)

// scriptedDaraja is a DarajaServiceHandler whose Relay sends whatever is
// pushed onto its send channel, and can be closed to simulate a disconnect.
// It lets a test drive the exact sequence of events a Runner must react to.
type scriptedDaraja struct {
	send chan *darajapb.RelayResponse
}

func newScriptedDaraja() *scriptedDaraja {
	return &scriptedDaraja{send: make(chan *darajapb.RelayResponse, 8)}
}

func (d *scriptedDaraja) Relay(ctx context.Context, stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse]) error {
	for {
		select {
		case resp, ok := <-d.send:
			if !ok {
				return nil // simulated disconnect: end the stream cleanly
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *scriptedDaraja) Restart(context.Context, *connect.Request[darajapb.RestartRequest]) (*connect.Response[darajapb.RestartResponse], error) {
	return connect.NewResponse(&darajapb.RestartResponse{}), nil
}

func (d *scriptedDaraja) Shutdown(context.Context, *connect.Request[darajapb.ShutdownRequest]) (*connect.Response[darajapb.ShutdownResponse], error) {
	return connect.NewResponse(&darajapb.ShutdownResponse{ExitCode: 7, Signal: "TERM"}), nil
}

func (d *scriptedDaraja) Health(context.Context, *connect.Request[darajapb.HealthRequest]) (*connect.Response[darajapb.HealthResponse], error) {
	return connect.NewResponse(&darajapb.HealthResponse{Running: true}), nil
}

func stdout(b []byte) *darajapb.RelayResponse {
	return &darajapb.RelayResponse{Event: &darajapb.RelayResponse_Stdout{Stdout: b}}
}

func exited(code int32, signal string) *darajapb.RelayResponse {
	return &darajapb.RelayResponse{Event: &darajapb.RelayResponse_Exited{
		Exited: &darajapb.ProcessExited{ExitCode: code, Signal: signal},
	}}
}

func restarted(pid int32) *darajapb.RelayResponse {
	return &darajapb.RelayResponse{Event: &darajapb.RelayResponse_Restarted{
		Restarted: &darajapb.ProcessRestarted{Pid: pid},
	}}
}

func TestRunnerRelaysStdoutFromWatchEvents(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	_, stdoutR, _, err := r.Start()
	c.NoError(err, "Start")

	stub.send <- stdout([]byte("hello "))
	stub.send <- stdout([]byte("world"))

	got := make([]byte, len("hello world"))
	if _, err := io.ReadFull(stdoutR, got); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	c.Eq("hello world", string(got), "got %q, want", got)
}

// TestRunnerMarksResetPendingOnRestartedEvent proves the wiring
// Controller.handleDarajaClaudeAbort's Pool.Restart call and daraja's own
// spontaneous crash-respawn both rely on: a RelayResponse_Restarted event
// sets the flag, TakeResetPending clears it exactly once, and — the part
// that would silently reopen the "translator state not reset" gap — the
// flag stays false absent any restart, and false again after being taken.
func TestRunnerMarksResetPendingOnRestartedEvent(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	_, stdoutR, _, err := r.Start()
	c.NoError(err, "Start")

	c.False(r.TakeResetPending(), "reset pending before any Restarted event")

	stub.send <- restarted(4242)
	// Stdout after the marker proves the pump kept relaying rather than
	// treating Restarted like Exited; it also gives the test a synchronization
	// point so the read below cannot race the switch case above.
	stub.send <- stdout([]byte("after-restart"))
	got := make([]byte, len("after-restart"))
	if _, err := io.ReadFull(stdoutR, got); err != nil {
		t.Fatalf("read stdout: %v", err)
	}

	c.True(r.TakeResetPending(), "want reset pending true after a Restarted event")
	c.False(r.TakeResetPending(), "TakeResetPending must clear the flag, not just read it")
}

func TestRunnerWaitReturnsOnExitedEvent(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	_, _, _, err := r.Start()
	c.NoError(err, "Start")

	stub.send <- exited(3, "")

	waitDone := make(chan struct{})
	var code int
	var signal string
	go func() {
		code, signal = r.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after an Exited event")
	}
	c.False(code != 3 || signal != "", "got (%d, %q), want (3, \"\")", code, signal)
}

func TestRunnerSurvivesADisconnectWithoutReportingExit(t *testing.T) {
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	_, stdoutR, _, err := r.Start()
	assert.NewAborting(t).NoError(err, "Start")

	// Simulate a disconnect: close the daraja's send side, which ends the
	// scriptedDaraja.Relay call and tears down the relay holder from the
	// pool's side. Wait() must NOT return.
	close(stub.send)

	waitDone := make(chan struct{})
	go func() {
		r.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait() returned after a mere disconnect — this must never look like the process exiting")
	case <-time.After(300 * time.Millisecond):
		// Expected: still blocked.
	}

	// The pump should be retrying Watch; without a real reconnect there is
	// nothing more to assert here (this package's reconnect mechanics are
	// pool-level, proven elsewhere — TestDisplacedConnectionDoesNotReportDisconnect
	// et al.). Confirm the stdout reader is still open and simply idle.
	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		_, _ = stdoutR.Read(buf) // blocks; just proves the pipe wasn't closed
		close(readDone)
	}()
	select {
	case <-readDone:
		t.Fatal("stdout reader returned — the pipe was closed on a mere disconnect")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRunnerTerminateCallsShutdownAndUnblocksWait(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	if _, _, _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c.NoError(r.Terminate(), "Terminate")

	waitDone := make(chan struct{})
	var code int
	var signal string
	go func() {
		code, signal = r.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after Terminate")
	}
	c.False(code != 7 || signal != "TERM", "got (%d, %q), want (7, \"TERM\") from scriptedDaraja.Shutdown's response", code, signal)
}

// TestClosingStdinTriggersShutdownAndUnblocksWait is the regression test for
// the bug found driving a real daemon end to end: pkg/child.Child.Shutdown
// closes stdin FIRST and only escalates to Terminate() after waiting out the
// full shutdownTimeout (which defaults to 180s — see Controller.Kill). A
// no-op Close left every kill of a daraja-hosted claude child silently idle
// for that whole window before anything actually happened.
func TestClosingStdinTriggersShutdownAndUnblocksWait(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	stdin, _, _, err := r.Start()
	c.NoError(err, "Start")

	c.NoError(stdin.Close(), "stdin.Close")

	waitDone := make(chan struct{})
	var code int
	var signal string
	go func() {
		code, signal = r.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after closing stdin — a real kill would have sat idle for the full shutdownTimeout")
	}
	c.False(code != 7 || signal != "TERM", "got (%d, %q), want (7, \"TERM\") from scriptedDaraja.Shutdown's response", code, signal)
}

func TestRunnerInterruptReturnsErrInterruptNotSupported(t *testing.T) {
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	err := r.Interrupt()
	assert.NewAborting(t).False(err != ErrInterruptNotSupported, "got %v, want ErrInterruptNotSupported", err)
}

// stderrEvent builds a stderr relay response — a script's stderr reaches the
// controller (its tail is what a failed run's settle carries); claude's never
// does.
func stderrEvent(b []byte) *darajapb.RelayResponse {
	return &darajapb.RelayResponse{Event: &darajapb.RelayResponse_Stderr{Stderr: b}}
}

// TestRunnerRelaysScriptStderr pins the script-hosted stderr leg: Stderr
// events arrive on the runner's stderr reader, interleaved with stdout on
// their own channel, and neither one's bytes appear on the other's pipe.
func TestRunnerRelaysScriptStderr(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newScriptedDaraja()
	pool, childID, teardown := connectFakeDaraja(t, stub)
	defer teardown()

	r := NewRunner(pool, childID)
	_, stdoutR, stderrR, err := r.Start()
	c.NoError(err, "Start")

	stub.send <- stdout([]byte("out-1"))
	stub.send <- stderrEvent([]byte("err-1"))
	stub.send <- stdout([]byte("out-2"))

	// Both pipes are synchronous: a consumer that reads one to EOF before
	// touching the other deadlocks, exactly as it would on the child side
	// (pkg/child drains stdout and stderr on their own goroutines). Read them
	// concurrently, the way the real consumer does.
	type result struct {
		name string
		data string
		err  error
	}
	results := make(chan result, 2)
	read := func(name string, rd io.Reader, n int) {
		b := make([]byte, n)
		_, err := io.ReadFull(rd, b)
		results <- result{name: name, data: string(b), err: err}
	}
	go read("stderr", stderrR, len("err-1"))
	go read("stdout", stdoutR, len("out-1out-2"))

	for range 2 {
		select {
		case res := <-results:
			c.NoError(res.err, "read %s", res.name)
			want := "out-1out-2"
			if res.name == "stderr" {
				want = "err-1"
			}
			c.Eq(want, res.data, "%s = %q, want", res.name, res.data)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout reading the relayed streams")
		}
	}
}
