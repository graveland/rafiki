package main

import (
	"errors"

	"go.graveland.dev/rafiki/pkg/execpool"
)

// mayMigrate reports whether a child bound to an executor in the given
// workspace mode may be re-bound to a DIFFERENT executor.
//
// Pinned means the child fails where it stood; ephemeral means the daemon may
// move it. An unknown or absent mode is pinned -- moving a child onto a machine
// no operator marked interchangeable is worse than failing it.
//
// This does not gate re-provisioning on the SAME executor: the executor keeps
// its workspace registry in memory, so a restart loses every id while the
// machine is perfectly fine, and rebuilding in place is not a migration.
func mayMigrate(mode string) bool {
	return workspaceModeOrPinned(mode) == "ephemeral"
}

// idempotentTools are the workspace verbs that can be re-dispatched after a
// stream break without risking doing their work twice.
//
// Read-only ONLY, and verified against the actual registrations in
// pkg/fundi/tools/executor_routing.go (tierByTool), not guessed from naming
// convention:
//   - read, glob, grep, ls: pure filesystem reads.
//   - lsp_call_hierarchy, lsp_definition, lsp_diagnostics, lsp_references,
//     lsp_symbols: query the language server, no workspace mutation.
//
// Deliberately absent:
//   - write, edit, bash, bash_start, bash_kill: side-effecting by
//     definition -- re-running "git push && rm -rf build" because the stream
//     died mid-response is the exact bug this set exists to prevent.
//   - lsp_rename: WRITES to every file in the workspace (its own tool
//     description says so). The one name in the original brief's example set
//     that would have been a data-loss bug had it shipped.
//   - lsp_restart: does not touch files, but it does restart the language
//     server process -- a "maybe ran" retry could kill a server that had
//     already restarted and was mid-index. Not a pure read, left out.
//   - lsp_hover, lsp_completion: do not exist as registered tools (checked
//     against pkg/fundi/tools' lsp_*.go files). Omitted rather than left in
//     as dead entries; a name that doesn't exist is silently non-idempotent
//     either way, so this is cosmetic, not a safety fix.
var idempotentTools = map[string]bool{
	"read": true, "glob": true, "grep": true, "ls": true,
	"lsp_call_hierarchy": true, "lsp_definition": true, "lsp_diagnostics": true,
	"lsp_references": true, "lsp_symbols": true,
}

func isIdempotentTool(name string) bool { return idempotentTools[name] }

// retryable decides whether a failed call may be re-dispatched.
//
//	pre-dispatch (nothing was sent)  -> any tool
//	stream broke (it may have run)   -> idempotent tools only
//	the tool ran and failed          -> never
func retryable(err error, idempotent bool) bool {
	if errors.Is(err, execpool.ErrToolFailed) {
		return false
	}
	if errors.Is(err, execpool.ErrStreamBroken) {
		return idempotent
	}
	return isLivenessFailure(err)
}
