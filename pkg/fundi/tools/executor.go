package tools

import (
	"context"
	"encoding/json"
)

// JobSnapshot is one poll of a background job.
type JobSnapshot struct {
	// Data is the output from the requested offset to Total.
	Data string
	// Total is the bytes the job has ever written. Pass it back as the next
	// poll's offset.
	Total int64
	// Exited is true once the process has been reaped.
	Exited bool
	// ExitCode is meaningful only when Exited.
	ExitCode int
	// Found is false when the handle is unknown — never started, or reaped
	// after the executor's retention window.
	Found bool
}

// ExecutorClient dispatches a tool call to an executor process.
//
// It is declared here, in the consumer, rather than in the executor client
// package: the tool layer is what needs the abstraction, and defining it
// here is what lets tests substitute an in-memory fake without importing a
// transport.
//
// A tool belongs behind this interface if and only if it touches
// machine-local resources a grant must constrain — the filesystem and shell
// tools. Everything holding a credential or a database pool (web_fetch,
// web_search, the task tools, MCP dispatch) stays in the parent, which is
// what keeps secrets above the boundary.
type ExecutorClient interface {
	Execute(ctx context.Context, tool string, input json.RawMessage) (string, error)

	// StartJob launches command in the background and returns a handle. It
	// returns as soon as the process is running — never after it finishes.
	StartJob(ctx context.Context, command string) (handle string, err error)
	// JobOutput polls a background job. It never blocks.
	JobOutput(ctx context.Context, handle string, since int64) (JobSnapshot, error)
	// KillJob terminates a background job and everything it spawned.
	KillJob(ctx context.Context, handle string) error

	// Ping verifies the executor is reachable and responsive. It must fail
	// when the executor is unreachable — no lazy connection.
	Ping(ctx context.Context) error
}
