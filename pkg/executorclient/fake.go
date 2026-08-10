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
	mu     sync.Mutex
	calls  []Call
	results   map[string]string
	failures  map[string]*executorpb.Failure
}

// NewFake returns a Fake with no pre-configured responses.
func NewFake() *Fake {
	return &Fake{
		results:  make(map[string]string),
		failures: make(map[string]*executorpb.Failure),
	}
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
