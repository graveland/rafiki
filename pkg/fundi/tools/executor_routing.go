package tools

// RoutedToExecutor returns the set of tool names that should be dispatched
// to an executor process when one is configured. These are the machine-local
// tools — filesystem and shell operations, plus background job lifecycle.
//
// Everything else stays in the parent: skill (inventory already loaded),
// task_* (DB pool, cross-agent reads), web_fetch/web_search (credentials),
// lsp_* (language server state), and every MCP tool (dispatched to
// daemon-managed hosts).
func RoutedToExecutor() []string {
	return []string{
		"read", "write", "edit", "glob", "grep",
		"bash", "bash_start", "bash_output", "bash_kill",
	}
}
