package child

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resettableStubRunner is a Runner backed by an io.Pipe (so the test controls
// exactly when each stdout line arrives) that also implements the optional
// pendingResetChecker interface resetProviderIfDue looks for — proving the
// real wiring between darajapool.Runner and child.Child end to end, without
// needing a live daraja connection.
type resettableStubRunner struct {
	stdoutW *io.PipeWriter
	stdoutR *io.PipeReader

	resetPending atomic.Bool

	mu     sync.Mutex
	waited bool
}

func (s *resettableStubRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	inR, inW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, inR) }()
	return inW, s.stdoutR, io.NopCloser(strings.NewReader("")), nil
}

func (s *resettableStubRunner) Wait() (int, string) {
	s.mu.Lock()
	s.waited = true
	s.mu.Unlock()
	return 0, ""
}

func (s *resettableStubRunner) PID() int         { return 0 }
func (s *resettableStubRunner) Terminate() error { return s.stdoutW.Close() }
func (s *resettableStubRunner) Kill() error      { return s.stdoutW.Close() }
func (s *resettableStubRunner) Interrupt() error { return nil }

// TakeResetPending implements the optional interface resetProviderIfDue
// checks for. Test-and-clear, matching darajapool.Runner's real semantics.
func (s *resettableStubRunner) TakeResetPending() bool { return s.resetPending.Swap(false) }

func newResettableStubRunner() *resettableStubRunner {
	r, w := io.Pipe()
	return &resettableStubRunner{stdoutR: r, stdoutW: w}
}

// TestResetProviderIfDueClearsStaleStateBetweenFrames drives two claude
// "processes" through ONE Child object with a reset flagged in between —
// exactly the daraja-Restart scenario resetProviderIfDue exists for — and
// proves the claude translator does not carry stale turnActive/model state
// from the first process into the second's frames.
func TestResetProviderIfDueClearsStaleStateBetweenFrames(t *testing.T) {
	stub := newResettableStubRunner()
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_reset_test",
		Cwd:      t.TempDir(),
		Runner:   stub,
		Provider: ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	write := func(line string) {
		if _, err := stub.stdoutW.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write stdout: %v", err)
		}
	}
	readUntilAgentStart := func(wantModel string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case f := <-busCh:
				var hdr struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(f, &hdr) == nil && hdr.Type == "agent_start" {
					return
				}
			case <-deadline:
				t.Fatalf("no agent_start observed for model %q", wantModel)
			}
		}
	}

	// First "process": establish a turn (turnActive=true in the translator).
	write(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-8"}`)
	write(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"hi"}]}}`)
	readUntilAgentStart("claude-opus-4-8")

	// Simulate a daraja Restart: the underlying process is replaced, and the
	// pending-reset flag is set BEFORE the replacement's first stdout byte —
	// exactly the ordering darajapool.Runner's own switch case guarantees.
	stub.resetPending.Store(true)

	// Second "process": if ResetState never ran, turnActive is still true and
	// openTurn's guard suppresses agent_start for this frame — the exact bug
	// this wiring exists to prevent.
	write(`{"type":"system","subtype":"init","session_id":"sess-2","model":"claude-sonnet-5"}`)
	write(`{"type":"assistant","session_id":"sess-2","message":{"content":[{"type":"text","text":"hi again"}]}}`)
	readUntilAgentStart("claude-sonnet-5")
}
