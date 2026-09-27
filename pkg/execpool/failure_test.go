package execpool

import (
	"errors"
	"fmt"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

func TestFailureErrorClassifiesToolFailure(t *testing.T) {
	err := failureError(&executorpb.Failure{
		Code:    executorpb.Failure_CODE_TOOL_FAILED,
		Message: "exit status 1",
	})
	assert.NewAborting(t).ErrorIs(err, ErrToolFailed, "a tool that ran and failed must be ErrToolFailed, got")
	if errors.Is(err, ErrExecutorGone) {
		t.Fatal("a tool failure must never look like a departed executor: " +
			"that would migrate a child every time bash exits nonzero")
	}
}

func TestFailureErrorClassifiesExecutorLost(t *testing.T) {
	err := failureError(&executorpb.Failure{
		Code:    executorpb.Failure_CODE_EXECUTOR_LOST,
		Message: `unknown workspace "ws-123"`,
	})
	assert.NewAborting(t).ErrorIs(err, ErrExecutorGone, "CODE_EXECUTOR_LOST must be ErrExecutorGone, got")
}

func TestFailureErrorKeepsTheMessage(t *testing.T) {
	c := assert.NewAborting(t)
	err := failureError(&executorpb.Failure{
		Code:    executorpb.Failure_CODE_DENIED,
		Message: "permission denied",
	})
	c.StrContains(err.Error(), "permission denied", "the executor's message must survive")
	c.ErrorIs(err, ErrToolFailed, "DENIED is a real answer from a live executor, not a departure")
}

func TestFailureErrorUnspecifiedIsNotADeparture(t *testing.T) {
	err := failureError(&executorpb.Failure{Code: executorpb.Failure_CODE_UNSPECIFIED})
	if errors.Is(err, ErrExecutorGone) {
		t.Fatal("an unrecognized code must NOT trigger rebinding: " +
			"failing toward 'stay put' costs one error, failing toward " +
			"'move' churns workspaces on every unknown failure")
	}
}

// A stream that opened and then broke is neither a tool failure nor a clean
// departure -- the tool may have already run. It must carry its own sentinel,
// distinct from both.
func TestErrStreamBrokenIsDistinctFromToolFailed(t *testing.T) {
	c := assert.NewAborting(t)
	err := fmt.Errorf("read: %w", ErrStreamBroken)
	c.ErrorIs(err, ErrStreamBroken, "want errors.Is match on ErrStreamBroken, got")
	c.False(errors.Is(err, ErrToolFailed), "a broken stream must not look like a tool that ran and failed")
	c.False(errors.Is(err, ErrExecutorGone), "a broken stream must not look like a clean departure either")
}

// A failure to even open the stream is distinct from one that opened and then
// broke: nothing was sent, so it must never look like "maybe ran".
// workspaceClient.Execute and executorClient.Execute wrap exactly this shape
// -- an error from c.inner.Execute() itself, before any Receive -- with both
// the raw error and this sentinel via double %w, which is what the test
// reproduces here.
func TestErrDialFailedIsDistinctFromStreamBroken(t *testing.T) {
	c := assert.NewAborting(t)
	raw := errors.New("write tcp 127.0.0.1:9-> 10.0.0.1:41: write: broken pipe")
	err := fmt.Errorf("executor execute: %w: %w", raw, ErrDialFailed)
	c.ErrorIs(err, ErrDialFailed, "want errors.Is match on ErrDialFailed, got")
	if errors.Is(err, ErrStreamBroken) {
		t.Fatal("a pre-dispatch failure must not look like a mid-stream break -- " +
			"that would refuse to retry side-effecting tools for no reason")
	}
	c.False(errors.Is(err, ErrToolFailed), "a pre-dispatch failure must not look like a tool that ran and failed")
	c.ErrorIs(err, raw, "the underlying transport error must survive for logging")
}
