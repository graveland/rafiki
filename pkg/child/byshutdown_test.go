package child

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// stopRunner drives one exit through Child.Shutdown with a controllable
// cause. Its stdin writer is active (a StdinStopper, the darajaStdin shape)
// or passive (a plain pipe), its Terminate releases the exit (the SIGTERM
// rung), and the exit itself is whatever (code, signal) the test names — so
// one type covers every row of the causality rule: a claude child exiting 143
// from the SIGTERM its coordinator's kill drove, a crash landing during the
// passive stdin-close wait, and a ladder escalation.
type stopRunner struct {
	active bool
	code   int
	signal string

	mu      sync.Mutex
	stdoutW io.WriteCloser

	release chan struct{}
	once    sync.Once
}

func newStopRunner(active bool, code int, signal string) *stopRunner {
	return &stopRunner{active: active, code: code, signal: signal, release: make(chan struct{})}
}

// releaseExit ends the child: unblock Wait, then close stdout so readStdout
// reaches EOF and reaps. The order matters — Wait must be unblocked before
// the reap calls it.
func (r *stopRunner) releaseExit() {
	r.once.Do(func() { close(r.release) })
	r.mu.Lock()
	w := r.stdoutW
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

func (r *stopRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	pr, pw := io.Pipe()
	r.mu.Lock()
	r.stdoutW = pw
	r.mu.Unlock()
	// Drain the pipe so a writer is never blocked on it.
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	var stdin io.WriteCloser
	if r.active {
		stdin = &stopStdin{runner: r}
	} else {
		ir, iw := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, ir) }()
		stdin = iw
	}
	return stdin, pr, io.NopCloser(strings.NewReader("")), nil
}

func (r *stopRunner) Wait() (int, string) {
	<-r.release
	return r.code, r.signal
}

func (r *stopRunner) PID() int         { return 0 }
func (r *stopRunner) Terminate() error { r.releaseExit(); return nil }
func (r *stopRunner) Kill() error      { r.releaseExit(); return nil }
func (r *stopRunner) Interrupt() error { return nil }

// stopStdin is the active closer: closing it IS the stop request, and the
// "process" ends because of it — exactly darajapool's darajaStdin.
type stopStdin struct{ runner *stopRunner }

func (s *stopStdin) Write(p []byte) (int, error) { return len(p), nil }
func (s *stopStdin) Close() error {
	s.runner.releaseExit()
	return nil
}
func (s *stopStdin) StopsOnClose() bool { return true }

// TestShutdownRecordsByShutdownForAnActiveStdinStop pins the causality bit
// for the daraja-hosted claude shape: the stdin closer is itself the stop
// request, the "process" answers with claude's handled-SIGTERM exit (143, no
// signal), and ExitResult must say the shutdown drove the death — not that a
// foreign crash landed.
func TestShutdownRecordsByShutdownForAnActiveStdinStop(t *testing.T) {
	r := newStopRunner(true, 143, "")
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_byshut", Cwd: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := c.Shutdown(time.Second, time.Second)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if res.ExitCode != 143 || res.Signal != "" {
		t.Fatalf("Shutdown = %+v, want the (143, \"\") shape", res)
	}
	if !res.ByShutdown {
		t.Fatal("an active stdin stop must record ByShutdown — its death is the shutdown's own doing")
	}
	if got := c.ExitResult(); !got.ByShutdown {
		t.Fatalf("ExitResult() = %+v, want ByShutdown on the stored record", got)
	}
}

// TestShutdownLeavesByShutdownOffForAPassiveCrash is the other half: a child
// that dies of its own accord (a crash, exit 1) during the passive stdin
// close's wait must NOT read as the shutdown's doing — that is the
// news-worthier death the suppression must keep notifying about.
func TestShutdownLeavesByShutdownOffForAPassiveCrash(t *testing.T) {
	r := newStopRunner(false, 1, "")
	go time.AfterFunc(50*time.Millisecond, r.releaseExit)
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_crash", Cwd: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := c.Shutdown(time.Second, time.Second)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if res.ExitCode != 1 || res.Signal != "" {
		t.Fatalf("Shutdown = %+v, want the crash shape", res)
	}
	if res.ByShutdown {
		t.Fatal("a spontaneous crash during the passive wait must not be attributed to the shutdown")
	}
	if got := c.ExitResult(); got.ByShutdown {
		t.Fatalf("ExitResult() = %+v, want ByShutdown clear", got)
	}
}

// TestShutdownRecordsByShutdownForAnEscalation pins the rung attribution: a
// passive stdin close that does not end the child escalates to SIGTERM, and
// the death the rung produces — here the (0, "terminated") shape of a process
// dying by signal — is the shutdown's own doing.
func TestShutdownRecordsByShutdownForAnEscalation(t *testing.T) {
	r := newStopRunner(false, 0, "terminated")
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_esc", Cwd: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := c.Shutdown(10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !res.Escalated || res.Signal != "terminated" {
		t.Fatalf("Shutdown = %+v, want the escalated (0, \"terminated\") shape", res)
	}
	if !res.ByShutdown {
		t.Fatal("an escalated rung's death must record ByShutdown")
	}
}
