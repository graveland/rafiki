package tools

// ExecutorLocalTools returns the tool names an executor process actually
// RUNS. It is the executor's whole surface: everything else — the task_*
// tools (a DB pool), web_fetch and web_search (credentials), skill (an
// inventory the parent already loaded), lsp_* and every MCP tool — stays in
// the parent, which is what keeps secrets above the boundary.
func ExecutorLocalTools() []string {
	return []string{"read", "write", "edit", "glob", "grep", "bash"}
}

// RoutedToExecutor returns the tool names the PARENT dispatches to an
// executor when one is configured. It is ExecutorLocalTools plus the three
// background-job verbs, which are parent-side tools whose implementation is
// an RPC rather than a local call — they never reach the executor's own
// registry.
func RoutedToExecutor() []string {
	return append(ExecutorLocalTools(), "bash_start", "bash_output", "bash_kill")
}
