package executorclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// Compile-time interface check.
var _ tools.ExecutorClient = (*Fake)(nil)

// Call records a single dispatched tool invocation.
type Call struct {
	Tool  string
	Input json.RawMessage
}

// Fake is an in-memory tools.ExecutorClient for parent-side tests. It records
// every call and returns pre-configured results.
type Fake struct {
	mu        sync.Mutex
	calls     []Call
	results   map[string]string
	failures  map[string]*executorpb.Failure
	jobs      map[string]*fakeJob
	nextJobID int
	pingErr   error // non-nil simulates an unreachable executor
}

// NewFake returns a Fake with no pre-configured responses.
func NewFake() *Fake {
	return &Fake{
		results:  make(map[string]string),
		failures: make(map[string]*executorpb.Failure),
		jobs:     make(map[string]*fakeJob),
	}
}

// SetPingError configures Ping to return err.
func (f *Fake) SetPingError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = err
}

// SetResult configures the fake to return text for tool name on every future
// call.
func (f *Fake) SetResult(tool, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[tool] = text
}

// SetFailure configures the fake to return a typed Failure for tool name.
func (f *Fake) SetFailure(tool string, code executorpb.Failure_Code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[tool] = &executorpb.Failure{Code: code, Message: message}
}

// Execute dispatches the tool call, records it, and returns either the
// pre-configured result or failure.
func (f *Fake) Execute(_ context.Context, tool string, input json.RawMessage) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Tool: tool, Input: input})
	if fail, ok := f.failures[tool]; ok {
		f.mu.Unlock()
		return "", newError(fail)
	}
	text, ok := f.results[tool]
	f.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("fake executor: no result configured for %q", tool)
	}
	return text, nil
}

// Calls returns a copy of the recorded invocations.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// newError wraps a proto Failure as a Go error. The parent can use errors.As
// to distinguish typed failures.
func newError(f *executorpb.Failure) error {
	return &FailureError{Failure: f}
}

// FailureError wraps an executor Failure so the parent can type-switch on it.
type FailureError struct {
	Failure *executorpb.Failure
}

func (e *FailureError) Error() string {
	return fmt.Sprintf("executor: %s (code %v)", e.Failure.Message, e.Failure.Code)
}

type fakeJob struct {
	command  string
	data     string
	exited   bool
	exitCode int
	killed   bool
}

// StartJob records the command and returns a deterministic handle
// ("job-1", "job-2", ...), so tests can assert on handles without matching
// a random id.
func (f *Fake) StartJob(_ context.Context, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextJobID++
	handle := fmt.Sprintf("job-%d", f.nextJobID)
	f.jobs[handle] = &fakeJob{command: command}
	return handle, nil
}

// SetJobOutput configures what a handle's next poll returns.
func (f *Fake) SetJobOutput(handle, data string, exited bool, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[handle]
	if !ok {
		j = &fakeJob{}
		f.jobs[handle] = j
	}
	j.data, j.exited, j.exitCode = data, exited, exitCode
}

// JobOutput returns the configured snapshot, honouring `since` the same way
// the real executor does.
func (f *Fake) JobOutput(_ context.Context, handle string, since int64) (tools.JobSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[handle]
	if !ok {
		return tools.JobSnapshot{Found: false}, nil
	}
	total := int64(len(j.data))
	if since > total {
		since = total
	}
	return tools.JobSnapshot{
		Data:     j.data[since:],
		Total:    total,
		Exited:   j.exited,
		ExitCode: j.exitCode,
		Found:    true,
	}, nil
}

// KillJob marks a job killed. Killed returns whether it was.
func (f *Fake) KillJob(_ context.Context, handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[handle]; ok {
		j.killed = true
		j.exited = true
		j.exitCode = -1
	}
	return nil
}

// Ping returns the configured pingErr. nil means the executor is reachable.
func (f *Fake) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

// Killed reports whether KillJob was called for handle.
func (f *Fake) Killed(handle string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[handle]
	return ok && j.killed
}

// JobCommand returns the command StartJob was called with.
func (f *Fake) JobCommand(handle string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[handle]; ok {
		return j.command
	}
	return ""
}
