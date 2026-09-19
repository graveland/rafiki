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

	// ErrStreamBroken means the RPC stream opened and then failed. Unlike the
	// sentinels above, this does NOT say whether the tool ran: the executor may
	// have executed the command and lost the connection while reporting it.
	// Callers must treat it as "maybe ran" and refuse to re-dispatch anything
	// with side effects.
	ErrStreamBroken = errors.New("execpool: the executor stream broke mid-call")

	// ErrDialFailed means the call never reached the executor at all: the
	// client-side Send failed before the stream was established. connect-go's
	// CallServerStream only returns an error here for a failure on OUR side of
	// the wire -- a response the server already sent, even an early one, comes
	// back as io.EOF instead and is left for Receive/stream.Err to report as
	// ErrStreamBroken. So an error surfacing at this point never reached the
	// executor: every tool, including side-effecting ones, is safe to retry.
	//
	// This is the common shape of "the executor's TCP connection died" for a
	// boundExecutor holding a CACHED client: the cached client bypasses
	// Pool.ClientFor (and its typed ErrParked/ErrExecutorLost/ErrDraining
	// answers) entirely, so a plain broken-pipe/connection-reset error from a
	// dead connection would otherwise carry no sentinel at all and retryable
	// would refuse it -- which is exactly what left a child's workspace tools
	// permanently broken after its executor restarted, healed only by
	// restarting rafikid and rebuilding every binding from scratch.
	ErrDialFailed = errors.New("execpool: could not reach the executor")
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
