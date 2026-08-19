package tools

// lspUnavailable reports whether the LSP tools should decline to materialize.
//
// They need somewhere to run: either a local LSP client, or an executor that
// will run the language servers against the files it holds and answer the
// proxied call. With neither, the tools could only fail, and a tool that can
// only fail costs the model a turn to learn nothing.
//
// The executor case does not need opts.LSP to be set. MaterializeAll wraps a
// routed tool in an executorProxy, which replaces Execute entirely — the nil
// client the blueprint materialized with is never dereferenced. Name,
// Description and InputSchema, the only other methods the model sees, do not
// touch it.
func lspUnavailable(opts ToolOpts) bool {
	return opts.LSP == nil && opts.Executor == nil
}
