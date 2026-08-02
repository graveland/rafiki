package child

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// hangingRunner is the runner an in-process child can genuinely become: Kill()
// does everything it can (cancels the context, closes both pipe read ends) and
// Wait() STILL never returns, because the goroutine it is waiting on is parked
// in a syscall no context can interrupt — a tool subprocess ignoring signals,
// an HTTP call with no timeout.
//
// It is deliberately modelled on internal/inproc.Runner rather than on a
// minimal stub: real os.Pipes, so the test can prove the property that makes
// abandoning the wait safe (a late write from the leaked goroutine FAILS) and
// not merely that some bookkeeping flag was set.
type hangingRunner struct {
	stdinR, stdinW   *os.File
	stdoutR, stdoutW *os.File
	stderr           *closeCounter

	ctx    context.Context
	cancel context.CancelFunc

	never chan struct{} // never closed; this is what Wait() blocks on
}

func newHangingRunner() *hangingRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &hangingRunner{
		stderr: &closeCounter{Reader: strings.NewReader("")},
		ctx:    ctx,
		cancel: cancel,
		never:  make(chan struct{}),
	}
}

func (h *hangingRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	var err error
	if h.stdinR, h.stdinW, err = os.Pipe(); err != nil {
		return nil, nil, nil, err
	}
	if h.stdoutR, h.stdoutW, err = os.Pipe(); err != nil {
		return nil, nil, nil, err
	}
	return h.stdinW, h.stdoutR, h.stderr, nil
}

// Wait models the whole point of the fix: it never returns.
func (h *hangingRunner) Wait() (int, string) {
	<-h.never
	return 0, ""
}

func (h *hangingRunner) PID() int         { return 0 }
func (h *hangingRunner) Terminate() error { h.cancel(); return nil }
func (h *hangingRunner) Interrupt() error { return nil }

// Kill mirrors inproc.Runner.Kill exactly: cancel, then close the read end of
// both pipes so a wedged read or write cannot survive it. What it cannot do is
// make Wait() return.
func (h *hangingRunner) Kill() error {
	h.cancel()
	var errs []error
	for _, f := range []*os.File{h.stdinR, h.stdoutR} {
		if f == nil {
			continue
		}
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// TestShutdownAbandonsAnUnreapableChild is the guard for the bounded terminal
// wait. Before it, Shutdown's last rung was an unbounded `<-c.done` after
// Kill(): correct for a subprocess (SIGKILL always lands) and a permanent hang
// for an in-process child whose eng.Wait() is parked on an uninterruptible
// syscall. That hung `fundi kill` outright and made ShutdownAllChildren return
// at its global bound having reported nothing.
//
// Abandoning a goroutine is only acceptable if the child is left unable to do
// damage when that syscall finally returns, so this asserts the four
// preconditions individually rather than just "Shutdown returned":
// every stream it could write to is closed, its context is cancelled, it is
// recorded as exited, and nothing is left waiting on it.
func TestShutdownAbandonsAnUnreapableChild(t *testing.T) {
	h := newHangingRunner()
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_hang", Cwd: t.TempDir(), Runner: h})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Shrink only the terminal bound; the mechanism is what is under test, not
	// the ten seconds. Written before Shutdown is called, which is its only
	// reader.
	const bound = 300 * time.Millisecond
	c.abandonAfter = bound

	<-c.Ready()

	// 10ms + 10ms of ladder, then the abandon bound. Anything materially past
	// that means the wait is not actually bounded.
	start := time.Now()
	res, err := c.Shutdown(10*time.Millisecond, 10*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v; the terminal wait after Kill() is not bounded", elapsed)
	}

	// Reported honestly: a caller must be able to tell reaped from abandoned.
	if !res.Abandoned {
		t.Error("ShutdownResult.Abandoned is false; an unreaped child must not be reported as reaped")
	}
	if !res.Escalated {
		t.Error("ShutdownResult.Escalated is false; this shutdown went all the way to Kill()")
	}
	// Same wire shape a reaped forced stop produces (see processRunner.Wait and
	// inproc's killedSignal), so `fundi kill` answers the same question the
	// same way either way.
	if res.Signal != "killed" || res.ExitCode != 0 {
		t.Errorf("exit shape = code %d signal %q, want code 0 signal \"killed\"", res.ExitCode, res.Signal)
	}

	// (1) Every stream the leaked goroutine could write to is closed. This is
	// the assertion that matters: a write from the abandoned child must FAIL
	// rather than succeed into a daemon that has moved on. stdoutW is the
	// engine's Frontend.Emit target.
	if _, werr := h.stdoutW.Write([]byte("late frame\n")); werr == nil {
		t.Error("a write to the abandoned child's stdout succeeded; a late Frontend.Emit must fail")
	}
	// The daemon-side stderr handle is normally released by supervise's cleanup
	// block, which a leaked supervise (parked in wg.Wait()) never reaches.
	if got := h.stderr.count(); got != 1 {
		t.Errorf("stderr closed %d times, want exactly 1; abandon must release the handles supervise no longer will", got)
	}

	// (2) The context is cancelled, so no NEW work can start when the syscall
	// returns.
	if h.ctx.Err() == nil {
		t.Error("the child's context is not cancelled; abandoning it would let it start new work")
	}

	// (3) Recorded as exited: the daemon must stop treating it as live.
	select {
	case <-c.Done():
	default:
		t.Error("Done() is not closed; monitorChild would never run handleChildExit for this child")
	}
	if serr := c.Send([]byte(`{"type":"prompt","message":"hi"}`)); serr == nil {
		t.Error("Send accepted a frame for an abandoned child; it must be rejected as shutting down")
	}
	if got := c.ExitResult(); !got.Abandoned || got.Signal != "killed" {
		t.Errorf("ExitResult() = %+v, want the abandoned/killed record", got)
	}

	// (4) Nothing waits on the leaked goroutine again: a second Shutdown takes
	// the already-exited path and returns immediately rather than blocking on
	// the same reap.
	second := make(chan ShutdownResult, 1)
	go func() {
		r2, _ := c.Shutdown(time.Minute, time.Minute)
		second <- r2
	}()
	select {
	case r2 := <-second:
		if !r2.Abandoned {
			t.Errorf("second Shutdown reported %+v; the abandoned record must be sticky", r2)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second Shutdown blocked; something still waits on the leaked goroutine")
	}
}

// TestAbandonedRecordSurvivesALateReap covers the other half of "recorded as
// exited": if the syscall the leaked goroutine was stuck in finally returns,
// readStdout reaches its reap-and-record block for a child the daemon has
// already finished reporting on. It must not overwrite the published outcome,
// and supervise's own `close(c.done)` must not panic on the already-closed
// channel — which is why both closers go through closeDone.
func TestAbandonedRecordSurvivesALateReap(t *testing.T) {
	h := newHangingRunner()
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_late", Cwd: t.TempDir(), Runner: h})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	c.abandonAfter = 200 * time.Millisecond
	<-c.Ready()

	res, err := c.Shutdown(10*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !res.Abandoned {
		t.Fatalf("child was not abandoned (%+v); this test no longer covers the late-reap path", res)
	}

	// The straggler lands: Wait() returns, readStdout records, supervise runs
	// its cleanup and its deferred done-close.
	close(h.never)

	// Give the leaked goroutines room to finish. A panic on the double close
	// would take the test binary down, so reaching the assertion at all is
	// itself part of the guard.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.ExitResult(); got.Signal == "killed" {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		break
	}
	if got := c.ExitResult(); !got.Abandoned || got.Signal != "killed" {
		t.Errorf("ExitResult() = %+v after a late reap; the abandoned record must be final", got)
	}
}

// TestAbandonTimeoutDefault pins the production bound, which the two tests
// above deliberately shrink. 10s is the deliberate choice: orders of magnitude
// more than a real post-Kill reap needs, and 120+30+10 = 160s keeps the whole
// per-child ladder inside cmd/rafikid's 180s global shutdown bound.
func TestAbandonTimeoutDefault(t *testing.T) {
	if abandonTimeout != 10*time.Second {
		t.Errorf("abandonTimeout = %v, want 10s (see its doc comment for the derivation)", abandonTimeout)
	}
	c, err := Spawn(t.Context(), SpawnSpec{
		ChildID: "c_default", Cwd: t.TempDir(),
		Runner: &stubRunner{stdoutFrames: `{"type":"agent_end"}` + "\n"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if c.abandonAfter != abandonTimeout {
		t.Errorf("Spawn set abandonAfter = %v, want abandonTimeout (%v)", c.abandonAfter, abandonTimeout)
	}
	<-c.Done()
}
