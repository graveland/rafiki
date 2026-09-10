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

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
)

// PermissionModeBypass is claude's own spelling for the mode that has its own
// flag rather than a --permission-mode value. Exported because every path that
// builds a daemon-managed claude child — the local-subprocess mapping
// (ParamsFromSpawnRequest, here) and the daraja wire mapping
// (daraja.ClaudeParamsForRequest) — must state the mode in the same words, or
// the two paths launch children that answer permission prompts differently.
const PermissionModeBypass = "bypassPermissions"

// Mode selects the argv shape.
type Mode int

const (
	// ModeHeadless is the stream-json contract a daemon-managed child needs.
	ModeHeadless Mode = iota
	// ModeInteractive keeps the TTY: no -p, no stream-json, no
	// --dangerously-skip-permissions, no --disallowedTools. A human answers
	// permission prompts, and AskUserQuestion has a renderer.
	ModeInteractive
)

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
	// ExtraArgs is an operator escape hatch, appended after everything Build
	// emits so it can override anything above it (matches the
	// local-subprocess spawn path's existing last-flag-wins convention for
	// req.ExtraArgs). UserArgs is appended after it, last.
	ExtraArgs []string

	// Mode selects the argv shape. The zero value is headless, so an existing
	// caller that does not set it is unaffected.
	Mode Mode

	// MCPConfig is the inline MCP server JSON, emitted as --mcp-config=<value>.
	// The = form is required: the flag is variadic, so a two-element pair risks
	// swallowing whatever follows it.
	MCPConfig string

	// ModelArgs, when non-empty, REPLACES the plain --model pair Model would
	// otherwise emit. It carries the custom-model-option --model that matches
	// ANTHROPIC_CUSTOM_MODEL_OPTION, which is what makes a non-Anthropic model
	// acceptable to Claude Code's own client-side allowlist.
	ModelArgs []string

	// UserArgs is argv passed through from a human's command line. Interactive
	// only; appended last.
	UserArgs []string
}

// Build returns argv EXCLUDING the binary itself, matching
// child.SpawnSpec.Argv.
//
// The four base flags are the stream-json contract: -p makes claude headless,
// the two format flags select the newline-delimited JSON protocol, and
// --verbose is what makes it emit the per-turn frames rather than only a final
// result. Dropping any of them yields a child that runs and cannot be parsed.
// They — along with the permission default and the --disallowedTools pair —
// are emitted only in ModeHeadless: ModeInteractive keeps the TTY, a human
// answers permission prompts, and AskUserQuestion has a renderer.
func Build(p Params) []string {
	var argv []string
	if p.Mode == ModeHeadless {
		argv = append(argv,
			"-p",
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
		)
	}
	if len(p.ModelArgs) > 0 {
		argv = append(argv, p.ModelArgs...)
	} else if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if p.MCPConfig != "" {
		argv = append(argv, "--mcp-config="+p.MCPConfig)
	}
	if p.ResumeSession != "" {
		argv = append(argv, "--resume", p.ResumeSession)
	}
	if p.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", p.AppendSystemPrompt)
	}
	if p.Mode == ModeHeadless {
		switch p.PermissionMode {
		case "", PermissionModeBypass:
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
	}
	argv = append(argv, p.ExtraArgs...)
	argv = append(argv, p.UserArgs...)
	return argv
}

// ParamsFromSpawnRequest maps a daemon SpawnRequest and the proxy's argv
// decisions (proxyenv.ClaudeEnv's Values) onto Params. It is the local-subprocess
// spawn path's mapping — cmd/rafikid's buildClaudeArgv is exactly
// Build(ParamsFromSpawnRequest(req, vals)) and must stay a delegation, so the
// mapping is pinned by the same tests that drive the daraja path
// (test/integration's TestClaudeArgvIdenticalAcrossPaths) rather than drifting
// from it again.
//
// vals.MCPConfig is the BARE inline JSON — Build itself prepends the
// --mcp-config= prefix — and vals.ModelArgs, when non-empty, REPLACES the
// plain --model pair req.Model would otherwise emit, so a proxied child
// carries exactly one --model and one --mcp-config element.
func ParamsFromSpawnRequest(req protocol.SpawnRequest, vals proxyenv.Values) Params {
	return Params{
		Model:              req.Model,
		ResumeSession:      req.ResumeSession,
		PermissionMode:     PermissionModeBypass,
		AppendSystemPrompt: req.AppendSystemPrompt,
		ExtraArgs:          req.ExtraArgs,
		MCPConfig:          vals.MCPConfig,
		ModelArgs:          vals.ModelArgs,
	}
}
