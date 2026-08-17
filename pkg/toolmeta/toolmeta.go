// Package toolmeta holds the two things the agent loop and the tools it runs
// must agree on: the id of the tool call currently executing, carried in
// context, and the largest result a tool may return.
//
// It is a leaf package rather than part of pkg/agentloop for a dependency
// reason. pkg/fundi/tools needs both symbols, and importing pkg/agentloop to get
// them pulled the entire agent runtime — pkg/llm, pkg/store, and through them
// pgx — into every binary that merely runs tools, the executor included. The
// whole edge was a context accessor and a constant.
//
// Nothing here may grow a dependency. If something needs one, it belongs in
// pkg/agentloop instead.
package toolmeta

import "context"

// MaxToolResultSize is the blind, content-agnostic cap applied to every tool
// result by the agent loop.
//
// A tool that budgets its own output must reserve headroom BELOW this number:
// the clip cuts from the tail, so a tool whose own budget equals this one gets
// its trailing "here is how to get the rest" hint silently removed — exactly
// the failure the per-tool budgets exist to prevent.
const MaxToolResultSize = 50 * 1024

type toolCallIDKey struct{}

// WithToolCallID marks ctx as executing the given tool_use id. The agent loop
// sets it; tools read it back with ToolCallID.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallID returns the tool_use id of the call being executed on this context,
// or "" when called outside a tool execution.
func ToolCallID(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}
