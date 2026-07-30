package child

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}

	got := c.RingSnapshot()
	if len(got) != 2 {
		t.Fatalf("ring holds %d frames, want 2: %q", len(got), got)
	}
	if !strings.Contains(string(got[0]), "agent_start") {
		t.Errorf("first frame = %q, want agent_start", got[0])
	}
	if c.PID() != 0 {
		t.Errorf("PID() = %d, want 0 for an injected runner", c.PID())
	}

	stub.mu.Lock()
	started, waited := stub.started, stub.waited
	stub.mu.Unlock()
	if !started {
		t.Error("runner.Start was never called")
	}
	if !waited {
		t.Error("runner.Wait was never called")
	}
}

// TestSpawnWithoutRunnerStillRequiresBinary preserves the process path's
// contract: no runner and no binary is a spec error, not a panic.
func TestSpawnWithoutRunnerStillRequiresBinary(t *testing.T) {
	if _, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_nobin", Cwd: t.TempDir()}); err == nil {
		t.Fatal("expected an error when neither Runner nor PiBinary is set")
	}
}
