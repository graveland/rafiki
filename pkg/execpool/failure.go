package execpool

import (
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// Failure classification. The executor already answers this question on the
// wire — Failure.Code separates a tool that ran and failed from a workspace
// that is gone — and workspaceClient.Execute used to discard it, collapsing
// both into one opaque string.
//
// The distinction is what stops a child migrating to another machine every
// time `bash` exits nonzero.
var (
	// ErrToolFailed means the tool ran on a live executor and reported a
	// failure. The executor is fine. Never re-bind on this.
	ErrToolFailed = errors.New("execpool: the tool ran and reported a failure")

	// ErrExecutorGone means the executor could not serve the call because it
	// or its workspace no longer exists. The caller decides which: an
	// executor still in the pool's live set means the WORKSPACE went (the
	// executor's registry is in-memory and a restart loses it), so
	// re-provision on the same executor; one absent from the live set means
	// the executor itself went, so re-bind.
	//
	// Deliberately not disambiguated here by parsing the message: a string
	// prefix check would be one wording change away from silently
	// misclassifying, and pool liveness is a fact this package already holds.
	ErrExecutorGone = errors.New("execpool: the executor or its workspace no longer exists")
)

// failureError converts an executor's Failure into an error carrying one of
// the sentinels above.
//
// An UNRECOGNISED code maps to ErrToolFailed, not ErrExecutorGone. That is the
// safe direction: treating an unknown failure as a departure churns workspaces
// on every future code this daemon has not learned yet, while treating it as a
// tool failure costs exactly one surfaced error.
func failureError(f *executorpb.Failure) error {
	switch f.GetCode() {
	case executorpb.Failure_CODE_EXECUTOR_LOST:
		return fmt.Errorf("executor: %s: %w", f.GetMessage(), ErrExecutorGone)
	default:
		return fmt.Errorf("executor: %s: %w", f.GetMessage(), ErrToolFailed)
	}
}
