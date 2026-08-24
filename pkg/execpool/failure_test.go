package execpool

import (
	"errors"
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
