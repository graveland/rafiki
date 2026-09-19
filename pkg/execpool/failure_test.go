package execpool

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

func TestFailureErrorClassifiesToolFailure(t *testing.T) {
	err := failureError(&executorpb.Failure{
		Code:    executorpb.Failure_CODE_TOOL_FAILED,
		Message: "exit status 1",
	})
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("a tool that ran and failed must be ErrToolFailed, got %v", err)
	}
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
	if !errors.Is(err, ErrExecutorGone) {
		t.Fatalf("CODE_EXECUTOR_LOST must be ErrExecutorGone, got %v", err)
	}
}

func TestFailureErrorKeepsTheMessage(t *testing.T) {
	err := failureError(&executorpb.Failure{
		Code:    executorpb.Failure_CODE_DENIED,
		Message: "permission denied",
	})
	if got := err.Error(); !strings.Contains(got, "permission denied") {
		t.Fatalf("the executor's message must survive: %q", got)
	}
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("DENIED is a real answer from a live executor, not a departure: %v", err)
	}
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
	err := fmt.Errorf("read: %w", ErrStreamBroken)
	if !errors.Is(err, ErrStreamBroken) {
		t.Fatalf("want errors.Is match on ErrStreamBroken, got %v", err)
	}
	if errors.Is(err, ErrToolFailed) {
		t.Fatal("a broken stream must not look like a tool that ran and failed")
	}
	if errors.Is(err, ErrExecutorGone) {
		t.Fatal("a broken stream must not look like a clean departure either")
	}
}

// A failure to even open the stream is distinct from one that opened and then
// broke: nothing was sent, so it must never look like "maybe ran".
// workspaceClient.Execute and executorClient.Execute wrap exactly this shape
// -- an error from c.inner.Execute() itself, before any Receive -- with both
// the raw error and this sentinel via double %w, which is what the test
// reproduces here.
func TestErrDialFailedIsDistinctFromStreamBroken(t *testing.T) {
	raw := errors.New("write tcp 127.0.0.1:9-> 10.0.0.1:41: write: broken pipe")
	err := fmt.Errorf("executor execute: %w: %w", raw, ErrDialFailed)
	if !errors.Is(err, ErrDialFailed) {
		t.Fatalf("want errors.Is match on ErrDialFailed, got %v", err)
	}
	if errors.Is(err, ErrStreamBroken) {
		t.Fatal("a pre-dispatch failure must not look like a mid-stream break -- " +
			"that would refuse to retry side-effecting tools for no reason")
	}
	if errors.Is(err, ErrToolFailed) {
		t.Fatal("a pre-dispatch failure must not look like a tool that ran and failed")
	}
	if !errors.Is(err, raw) {
		t.Fatal("the underlying transport error must survive for logging")
	}
}
