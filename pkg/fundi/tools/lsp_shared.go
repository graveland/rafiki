package tools

// lspUnavailable reports whether the LSP tools should decline to materialize.
//
// Two reasons, and they are different. No LSP client means nothing to talk to.
// An executor means the workspace is on another machine: a language server
// started in this process would index the daemon's filesystem and answer about
// files the agent is not editing, and lsp_rename would write to them. Routing
// the tools to the executor is the real fix and needs the executor to host the
// LSP manager; until then, absent beats confidently wrong.
func lspUnavailable(opts ToolOpts) bool {
	return opts.LSP == nil || opts.Executor != nil
}
