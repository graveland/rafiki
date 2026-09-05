// SPDX-License-Identifier: Apache-2.0

// Package claudeargv builds claude's command line from typed parameters.
//
// It exists so there is exactly ONE builder. Three callers need it — the
// executor's AdminService when it launches a daraja, daraja itself when it
// restarts or respawns the child, and rafikid's own agentRunner when it
// spawns a claude child with no executor available — and all three live in
// the `rafiki`/`rafikid` binaries, so a single function here is one builder
// by construction rather than several kept in step by discipline.
package claudeargv

import "strings"

// bypassPermissions is claude's own spelling for the mode that has its own
// flag rather than a --permission-mode value.
const bypassPermissions = "bypassPermissions"

// Params is what a caller may vary. Everything else about claude's invocation
// is fixed by the protocol daraja relays.
type Params struct {
	Model              string
	ResumeSession      string
	PermissionMode     string
	AppendSystemPrompt string
	// DisallowedTools is appended alongside the always-disallowed
	// AskUserQuestion (see Build's doc comment) — never in place of it.
	DisallowedTools []string
	// ExtraArgs is an operator escape hatch, appended last so it can override
	// anything above it (matches the local-subprocess spawn path's existing
	// last-flag-wins convention for req.ExtraArgs).
	ExtraArgs []string
}

// Build returns argv EXCLUDING the binary itself, matching
// child.SpawnSpec.Argv.
//
// The four base flags are the stream-json contract: -p makes claude headless,
// the two format flags select the newline-delimited JSON protocol, and
// --verbose is what makes it emit the per-turn frames rather than only a final
// result. Dropping any of them yields a child that runs and cannot be parsed.
func Build(p Params) []string {
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if p.ResumeSession != "" {
		argv = append(argv, "--resume", p.ResumeSession)
	}
	if p.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", p.AppendSystemPrompt)
	}
	switch p.PermissionMode {
	case "", bypassPermissions:
		// A daemon-managed claude child has no human to answer an
		// interactive permission prompt, so bypass is the default, not an
		// opt-in — claude blocks forever in headless mode waiting for an
		// answer nobody can give otherwise.
		argv = append(argv, "--dangerously-skip-permissions")
	default:
		argv = append(argv, "--permission-mode", p.PermissionMode)
	}
	// AskUserQuestion has no interactive renderer in headless -p mode: claude
	// self-resolves it with an error in the same turn, then the agent falls
	// back to asking in prose, which round-trips fine over the prompt/steer
	// channel. Disallow the dead tool so it never wastes a turn attempting
	// it — always, regardless of what the caller passes in DisallowedTools.
	disallowed := append([]string{"AskUserQuestion"}, p.DisallowedTools...)
	argv = append(argv, "--disallowedTools", strings.Join(disallowed, ","))
	argv = append(argv, p.ExtraArgs...)
	return argv
}
