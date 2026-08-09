package tools

import (
	"context"
	"encoding/json"
)

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
}
