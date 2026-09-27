package child

import (
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// stubRunner is a Runner backed by in-memory streams. It proves Spawn honours
// an injected runner and never reaches exec.Command.
type stubRunner struct {
	stdoutFrames string

	mu      sync.Mutex
	started bool
	waited  bool
}

func (s *stubRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	// stdin: discard whatever the supervise loop writes.
	inR, inW := io.Pipe()
	go func() {
		if _, err := io.Copy(io.Discard, inR); err != nil {
			return // pipe closed on shutdown; nothing to report
		}
	}()

	stdout := io.NopCloser(strings.NewReader(s.stdoutFrames))
	stderr := io.NopCloser(strings.NewReader(""))
	return inW, stdout, stderr, nil
}

func (s *stubRunner) Wait() (int, string) {
	s.mu.Lock()
	s.waited = true
	s.mu.Unlock()
	return 0, ""
}

func (s *stubRunner) PID() int         { return 0 }
func (s *stubRunner) Terminate() error { return nil }
func (s *stubRunner) Kill() error      { return nil }
func (s *stubRunner) Interrupt() error { return nil }

// TestSpawnUsesInjectedRunner proves the seam is real: with a runner on the
// spec, Spawn must not exec anything, and frames from the runner's stdout must
// reach the ring exactly as they would from a process.
func TestSpawnUsesInjectedRunner(t *testing.T) {
	ck := assert.NewCollecting(t)
	stub := &stubRunner{
		stdoutFrames: `{"type":"agent_start"}` + "\n" + `{"type":"agent_end"}` + "\n",
	}

	// PiBinary is deliberately empty: an injected runner makes it irrelevant,
	// and a non-empty value would hide a fallback to exec.Command.
	c, err := Spawn(t.Context(), SpawnSpec{
		ChildID: "c_stub",
		Cwd:     t.TempDir(),
		Runner:  stub,
	})
	ck.Require().NoError(err, "Spawn")

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}

	got := c.RingSnapshot()
	ck.Require().Len(got, 2, "ring holds %d frames, want 2", len(got))
	ck.StrContains(string(got[0]), "agent_start", "first frame = %q, want agent_start", got[0])
	ck.Eq(0, c.PID(), "PID()")

	stub.mu.Lock()
	started, waited := stub.started, stub.waited
	stub.mu.Unlock()
	ck.True(started, "runner.Start was never called")
	ck.True(waited, "runner.Wait was never called")
}

// TestSpawnWithoutRunnerStillRequiresBinary preserves the process path's
// contract: no runner and no binary is a spec error, not a panic.
func TestSpawnWithoutRunnerStillRequiresBinary(t *testing.T) {
	_, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_nobin", Cwd: t.TempDir()})
	assert.NewAborting(t).Error(err, "expected an error when neither Runner nor PiBinary is set")
}

// TestSpawnInjectedRunnerSkipsCwdExistenceCheck proves the other half of the
// same seam TestSpawnUsesInjectedRunner covers: an injected Runner (the
// in-process fundi engine) never forks a real process rooted at Cwd, so a Cwd
// that does not exist on THIS machine must not refuse the spawn — its
// filesystem access, if any, goes through whichever executor gets bound
// later, not this one.
func TestSpawnInjectedRunnerSkipsCwdExistenceCheck(t *testing.T) {
	stub := &stubRunner{stdoutFrames: `{"type":"agent_end"}` + "\n"}
	c, err := Spawn(t.Context(), SpawnSpec{
		ChildID: "c_stub_bad_cwd",
		Cwd:     "/definitely/not/a/directory",
		Runner:  stub,
	})
	assert.NewAborting(t).NoError(err, "Spawn")
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}
}

// TestSpawnWithoutRunnerRejectsMissingCwd is the discriminator for the fix
// above: the subprocess path (no injected Runner) DOES fork a real process
// rooted at Cwd (newProcessRunner sets cmd.Dir = spec.Cwd), so a missing Cwd
// must still be refused there.
func TestSpawnWithoutRunnerRejectsMissingCwd(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := Spawn(t.Context(), SpawnSpec{
		ChildID:  "c_nocwd",
		Cwd:      "/definitely/not/a/directory",
		PiBinary: "/bin/true",
	})
	c.Require().Error(err, "expected an error for a Cwd that does not exist on the subprocess path")
	c.StrContains(err.Error(), "cwd", "error")
}

// closeCounter wraps a stream and counts Close calls, so a test can assert on
// the daemon's stream hygiene directly instead of inferring it from a process
// file-descriptor count. An FD count cannot be the guard here: os.File
// finalizers reclaim most of the leaked handles opportunistically during a
// test run, so the observed delta is a fraction of the real leak and depends
// on when the GC happened to run.
type closeCounter struct {
	io.Reader
	io.Writer
	mu     sync.Mutex
	closes int
}

func (c *closeCounter) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *closeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// recordingRunner hands back streams that record their own closes.
type recordingRunner struct {
	stdin  *closeCounter
	stdout *closeCounter
	stderr *closeCounter
}

func newRecordingRunner(stdoutFrames string) *recordingRunner {
	return &recordingRunner{
		stdin:  &closeCounter{Writer: io.Discard},
		stdout: &closeCounter{Reader: strings.NewReader(stdoutFrames)},
		stderr: &closeCounter{Reader: strings.NewReader("")},
	}
}

func (r *recordingRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	return r.stdin, r.stdout, r.stderr, nil
}
func (r *recordingRunner) Wait() (int, string) { return 0, "" }
func (r *recordingRunner) PID() int            { return 0 }
func (r *recordingRunner) Terminate() error    { return nil }
func (r *recordingRunner) Kill() error         { return nil }
func (r *recordingRunner) Interrupt() error    { return nil }

// TestSuperviseClosesChildOutputStreams is the regression guard for the
// daemon's FD hygiene. Until this existed, `c.stdin.Close()` in Shutdown was
// the ONLY close of any daemon-side stream handle anywhere in the daemon;
// c.stdout and c.stderr were never closed at all, left to os.File finalizers.
//
// Finalizers are not a backstop for this: FD exhaustion does not trigger a GC,
// so a daemon churning children reaches EMFILE with a perfectly small heap.
// In-process children make it far worse than theoretical, since self-exit (a
// failed Build, a frontend scan error, a contained panic) is their common case
// rather than an exception.
func TestSuperviseClosesChildOutputStreams(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := newRecordingRunner(`{"type":"agent_start"}` + "\n" + `{"type":"agent_end"}` + "\n")
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_fd", Cwd: t.TempDir(), Runner: r})
	ck.Require().NoError(err, "Spawn")
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}

	ck.Eq(1, r.stdout.count(), "stdout closed")
	ck.Eq(1, r.stderr.count(), "stderr closed")
}

// TestSuperviseClosesStdinOnSelfExit covers the third stream, on the path that
// needed it most.
//
// Child.Shutdown closes c.stdin — but handleChildExit does not call Shutdown;
// its only callers are Controller.Kill and ShutdownAllChildren. So a child that
// ended ON ITS OWN (a build error, an engine-fatal, a frontend EOF — the COMMON
// case for an in-process agent child, not an exception) left its stdin write
// end to an os.File finalizer, and FD exhaustion does not trigger a GC, so
// finalizers are not a backstop under real pressure. supervise's cleanup block
// is the symmetric home: it is the only writer to c.stdin and has already left
// the write loop.
//
// Then it checks the composition: Shutdown's own unconditional stdin close
// still runs afterwards and is tolerated rather than logged as a failure, which
// is what closeStream's os.ErrClosed handling is for. Both closes are wanted —
// Shutdown's is step 1 of the graceful ladder when it runs BEFORE the child
// exits.
func TestSuperviseClosesStdinOnSelfExit(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := newRecordingRunner(`{"type":"agent_end"}` + "\n")
	c, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_fd_stdin", Cwd: t.TempDir(), Runner: r})
	ck.Require().NoError(err, "Spawn")
	// Done() is closed by supervise's LAST deferred call, after its cleanup
	// block has run — so observing it means the closes have already happened.
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}
	if got := r.stdin.count(); got != 1 {
		t.Fatalf("stdin closed %d times on the self-exit path, want exactly 1; "+
			"nothing else in the daemon closes it for a child that was never Shutdown", got)
	}

	// The already-exited Shutdown path still closes unconditionally.
	if _, err := c.Shutdown(time.Second, time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := r.stdin.count()
	ck.Eq(2, got, "stdin closed %d times after a follow-up Shutdown, want 2 "+
		"(supervise's close plus Shutdown's own, which must tolerate the already-closed handle)", got)
}

// Setpgid is the default because a child that spawns subprocesses must be
// signallable as a group. daraja needs the opposite: its claude must JOIN
// daraja's group, so that one kill(-pgid) from the executor reaches both and
// keeps reaching claude after daraja is SIGKILLed.
//
// Nothing else observes this, so a regression is silent — hence a test that
// reads the real pgid out of the kernel.
func TestInheritProcessGroupPutsTheChildInOurGroup(t *testing.T) {
	c := assert.NewCollecting(t)
	r, err := newProcessRunner(SpawnSpec{
		PiBinary:            "/bin/cat",
		InheritProcessGroup: true,
	})
	c.Require().NoError(err, "newProcessRunner")
	stdin, _, _, err := r.Start()
	c.Require().NoError(err, "Start")
	defer func() { _ = stdin.Close(); _, _ = r.Wait() }()

	got, err := syscall.Getpgid(r.PID())
	c.Require().NoError(err, "Getpgid(%d)", r.PID())
	c.Eq(syscall.Getpgrp(), got, "child pgid")
	_ = os.Getpid()
}

func TestDefaultStillGivesTheChildItsOwnGroup(t *testing.T) {
	c := assert.NewCollecting(t)
	r, err := newProcessRunner(SpawnSpec{PiBinary: "/bin/cat"})
	c.Require().NoError(err, "newProcessRunner")
	stdin, _, _, err := r.Start()
	c.Require().NoError(err, "Start")
	defer func() { _ = stdin.Close(); _, _ = r.Wait() }()

	got, err := syscall.Getpgid(r.PID())
	c.Require().NoError(err, "Getpgid")
	c.Eq(r.PID(), got, "child pgid")
}

// A child that inherited our group must still be signallable.
//
// signalGroup targeted -pid, which names a process group only when the child
// LEADS one. daraja's claude joins daraja's group (see SpawnSpec), so -pid was
// ESRCH on every stop and the signal silently stopped nothing — daraja's
// stopLocked then blocked forever on the exit it was waiting for, hanging its
// Shutdown and Restart RPCs. The group the child belongs to is daraja's own
// and must not be signalled, so the pid itself is the only remaining target.
func TestInheritProcessGroupChildIsStillSignallable(t *testing.T) {
	c := assert.NewAborting(t)
	r, err := newProcessRunner(SpawnSpec{
		PiBinary:            "/bin/sh",
		Argv:                []string{"-c", "sleep 30"},
		InheritProcessGroup: true,
	})
	c.NoError(err, "newProcessRunner")
	if _, _, _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.NoError(r.Terminate(), "Terminate")

	// Without the pid fallback this never returns: SIGTERM went to a group
	// that does not exist, and /bin/sh outlives the test.
	reaped := make(chan struct{})
	go func() { _, _ = r.Wait(); close(reaped) }()
	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("child survived SIGTERM; an inherited-group child is not signallable")
	}
}
