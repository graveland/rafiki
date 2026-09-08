// Package mcpserver translates already-bound rafiki tools onto MCP.
//
// It takes materialized tools, not blueprints: materialization is where a
// caller's identity is bound, and that binding must happen in the daemon. New
// therefore builds a *mcp.Server whose every tool is already scoped to
// exactly one caller, so no method on it — or on anything it exposes —
// carries a caller identity.
package mcpserver
