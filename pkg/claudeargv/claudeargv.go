// SPDX-License-Identifier: Apache-2.0

// Package claudeargv builds claude's command line from typed parameters.
//
// It exists so there is exactly ONE builder. Two callers need it — the
// executor's AdminService when it launches a daraja, and daraja itself when it
// restarts or respawns the child — and both live in the `rafiki` binary, so a
// single function here is one builder by construction rather than two kept in
// step by discipline.
package claudeargv

// bypassPermissions is claude's own spelling for the mode that has its own
// flag rather than a --permission-mode value.
const bypassPermissions = "bypassPermissions"

// Params is what a caller may vary. Everything else about claude's invocation
// is fixed by the protocol daraja relays.
type Params struct {
	Model          string
	ResumeSession  string
	PermissionMode string
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
	switch p.PermissionMode {
	case "":
	case bypassPermissions:
		argv = append(argv, "--dangerously-skip-permissions")
	default:
		argv = append(argv, "--permission-mode", p.PermissionMode)
	}
	return argv
}
