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

// Result is what a tool hands back to the agent loop. Text is the common case;
// Images ride alongside the text in the SAME tool_result block — a `read` of a
// PNG returns one — and travel both into the model's live request and into the
// durable conversation (the message row and the event log), so a resumed agent
// still sees the image.
//
// It lives here, not in pkg/fundi/tools, for the same dependency reason this
// package exists: the loop and the tools must agree on the shape, and neither
// may import the other. Keep it a plain data type — no methods that need an
// import beyond context.
type Result struct {
	Text   string
	Images []Image
}

// Image is one image content block riding a tool result. MediaType is an IANA
// media type ("image/png"); Data is the raw bytes — base64 encoding is the
// transport's business, not the tool's.
type Image struct {
	MediaType string
	Data      []byte
}

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
