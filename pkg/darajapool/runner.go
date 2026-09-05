// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// ErrInterruptNotSupported is returned by Runner.Interrupt. A daraja-hosted
// claude's turn cannot yet be aborted without corrupting the claude
// translator's per-conversation state (the ProcessRestarted boundary marker
// this needs is not consumed anywhere yet — see the 1c plan's "Why this is
// scoped the way it is"). handleClaudeAbort already surfaces a Runner error
// as a clean RPC failure, so this refuses cleanly rather than silently
// restarting into a bad state.
var ErrInterruptNotSupported = errors.New("darajapool: interrupting a daraja-hosted claude child is not yet supported")

// watchRetryBackoff bounds how often Runner re-attempts Watch after a
// disconnect. Short enough that a fast reconnect (the common case) resumes
// output almost immediately; long enough that a genuinely gone daraja does
// not spin the daemon.
const watchRetryBackoff = 500 * time.Millisecond

// darajaShutdownTimeout bounds Terminate/Kill's own Shutdown RPC call — it
// must not hang the caller (e.g. Controller.Kill) indefinitely if the daraja
// connection is wedged rather than cleanly gone.
const darajaShutdownTimeout = 10 * time.Second

type exitInfo struct {
	code   int
	signal string
}

// Runner is a child.Runner backed by a live daraja connection in Pool. It
// never forks or launches anything itself — by the time NewRunner is called,
// Launch (this package) has already put a daraja+claude on some executor and
// the reverse dial has landed in Pool, keyed by childID.
//
// A daraja connection loss must NOT look like the child process exiting:
// WireDaraja already marks the child rafiki/daraja-state=unreachable on
// Pool.OnDisconnect, generic over any childID. child.Runner's contract
// assumes stdout EOF and Wait() returning both mean the process ended, so
// this Runner's stdout pump does not close the pipe or report exit on a mere
// disconnect — it retries Watch in a loop until either a fresh connection's
// events resume (the pool's own reconnect machinery re-establishes the relay
// holder transparently) or Terminate/Kill tells it to give up for good.
type Runner struct {
	pool    *Pool
	childID string

	pr *io.PipeReader
	pw *io.PipeWriter

	doneOnce sync.Once
	done     chan struct{} // closed by Terminate/Kill: tells the pump to stop retrying

	exitCh chan exitInfo

	waitOnce sync.Once
	waitInfo exitInfo
}

// NewRunner returns a Runner for a daraja already connected in pool under
// childID.
func NewRunner(pool *Pool, childID string) *Runner {
	pr, pw := io.Pipe()
	return &Runner{
		pool:    pool,
		childID: childID,
		pr:      pr,
		pw:      pw,
		done:    make(chan struct{}),
		exitCh:  make(chan exitInfo, 1),
	}
}

// darajaStdin adapts Pool.Send to io.WriteCloser. Close is a no-op: daraja's
// Relay stream stays open for the child's whole life (see Send's own doc
// comment), so there is nothing to close per-write, and closing it would
// prevent the next Send.
type darajaStdin struct {
	pool    *Pool
	childID string
}

func (s *darajaStdin) Write(p []byte) (int, error) {
	if err := s.pool.Send(s.childID, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *darajaStdin) Close() error { return nil }

func (r *Runner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	go r.pump()
	return &darajaStdin{pool: r.pool, childID: r.childID}, r.pr, io.NopCloser(bytes.NewReader(nil)), nil
}

// pump feeds r.pw from the child's relay events for as long as r.done is
// open, transparently re-subscribing across a disconnect/reconnect. It is
// the only writer to r.pw and the only sender to r.exitCh.
func (r *Runner) pump() {
	defer r.pw.Close()
	for {
		select {
		case <-r.done:
			return
		default:
		}

		events, unsub, err := r.pool.Watch(r.childID)
		if err != nil {
			// No connection right now (e.g. mid-reconnect). WireDaraja's
			// OnDisconnect callback already marked the child unreachable;
			// this loop's job is only to keep trying, not to report exit.
			select {
			case <-time.After(watchRetryBackoff):
				continue
			case <-r.done:
				return
			}
		}

		stop := r.drain(events)
		unsub()
		if stop {
			return
		}
		// events closed (holder torn down — disconnect). Loop and re-Watch.
	}
}

// drain consumes events until the channel closes or a terminal event
// arrives. Returns true when the child has genuinely exited (or Terminate/
// Kill fired) — the pump should stop for good; false when it should re-Watch
// (disconnect, not exit).
func (r *Runner) drain(events <-chan *fanEvent) bool {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return false // holder torn down: disconnect, not exit
			}
			if ev.Err() != nil {
				return false // stream error: treat the same as disconnect
			}
			switch e := ev.Response().GetEvent().(type) {
			case *darajapb.RelayResponse_Stdout:
				if _, err := r.pw.Write(e.Stdout); err != nil {
					return true // reader gone: Terminate/Kill closed things down
				}
			case *darajapb.RelayResponse_Exited:
				r.reportExit(int(e.Exited.ExitCode), e.Exited.Signal)
				return true
			case *darajapb.RelayResponse_Restarted:
				// 1c-ii: reset the claude translator's per-process state here
				// (the ProcessRestarted boundary marker). Not reachable by any
				// code path this Runner adds on purpose (Interrupt refuses
				// rather than calling Restart) but daraja's own crash-loop
				// recovery can still emit this spontaneously if claude dies on
				// its own. Log it so a spontaneous crash is at least visible;
				// keep relaying stdout on the same pipe without resetting
				// anything. A translator confused by stale state after a
				// spontaneous crash is the known, accepted gap 1c-i leaves
				// open — see the plan's "Why this is scoped the way it is".
				slog.Warn("daraja: child process was replaced (spontaneous restart); translator state not reset — see 1c-ii",
					"childId", r.childID, "newPid", e.Restarted.Pid)
			}
		case <-r.done:
			return true
		}
	}
}

func (r *Runner) reportExit(code int, signal string) {
	select {
	case r.exitCh <- exitInfo{code: code, signal: signal}:
	default:
		// Already reported (e.g. shutdown() raced an Exited event). First
		// one wins; Wait()'s sync.Once only ever reads the first.
	}
}

// Wait blocks until the child has genuinely exited (never on a mere
// disconnect) and reports the outcome. Called exactly once per Runner by
// pkg/child's supervise loop; sync.Once makes a second call return the same
// cached value rather than blocking forever on an already-drained channel.
func (r *Runner) Wait() (exitCode int, signal string) {
	r.waitOnce.Do(func() {
		r.waitInfo = <-r.exitCh
	})
	return r.waitInfo.code, r.waitInfo.signal
}

// PID reports 0: there is no OS process on this machine to name. The
// meaningful identifier — the daraja process group's pid, from the Launch
// result — is recorded as the rafiki/daraja-pgid label instead (see the 1c
// plan's Task 7).
func (r *Runner) PID() int { return 0 }

func (r *Runner) Terminate() error { return r.shutdown(3000) }
func (r *Runner) Kill() error      { return r.shutdown(0) }

func (r *Runner) shutdown(graceMs int32) error {
	r.doneOnce.Do(func() { close(r.done) })
	ctx, cancel := context.WithTimeout(context.Background(), darajaShutdownTimeout)
	defer cancel()
	code, signal, err := r.pool.Shutdown(ctx, r.childID, graceMs)
	if err != nil {
		// Unreachable (no live connection) — give up locally rather than
		// hang. -1 matches child.Runner's documented "could not be
		// determined" sentinel.
		r.reportExit(-1, "")
		return nil
	}
	r.reportExit(int(code), signal)
	return nil
}

func (r *Runner) Interrupt() error { return ErrInterruptNotSupported }

var _ child.Runner = (*Runner)(nil)
