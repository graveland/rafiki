package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
)

// printAgentUsage writes `rafikid fundi` usage. It exists because
// parseAgentFlags points its FlagSet at io.Discard so runAgent can report parse
// errors itself — which also swallowed the -h output, making `rafikid fundi -h`
// exit 0 having printed nothing.
func printAgentUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: rafikid fundi [flags]

Runs a single agent child speaking pi's rpc protocol on stdio, in place of
Claude Code. Normally spawned by the rafiki daemon rather than invoked directly.

Flags:
`)
	var f agentFlags
	fs := newAgentFlagSet(&f)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// newAgentFlagSet registers `rafikid fundi`'s flags. Shared by parseAgentFlags and
// printAgentUsage so the documented flags cannot drift from the parsed ones.
//
// Output goes to io.Discard: runAgent reports parse errors itself, and
// printAgentUsage redirects this to the real writer when it wants the defaults.
func newAgentFlagSet(f *agentFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&f.model, "model", "", "provider-qualified model id, e.g. \"anthropic/sonnet-latest\" or \"deepseek/deepseek-chat\" (required)")
	fs.StringVar(&f.thinking, "thinking", "off", "extended-thinking level: off|low|medium|high|xhigh")
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "override the base system prompt")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt")
	fs.BoolVar(&f.noContextFiles, "no-context-files", false, "skip loading CLAUDE.md/AGENTS.md context files")
	fs.Var(&f.skillsDir, "skills-dir", "additional skills directory (repeatable)")
	fs.StringVar(&f.skills, "skills", "", "comma-separated list restricting discovered skills to these names")
	fs.BoolVar(&f.noSkills, "no-skills", false, "disable skill discovery and the skill tool entirely")
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "path to .mcp.json (default: <cwd>/.mcp.json if present, else $RAFIKI_MCP_CONFIG or <ConfigDir>/mcp.json)")
	fs.StringVar(&f.lspConfig, "lsp-config", "", "path to lsp.json (default: <cwd>/.lsp.json if present, else $RAFIKI_LSP_CONFIG or <ConfigDir>/lsp.json)")
	fs.StringVar(&f.ref, "ref", paths.Get(paths.ChildID), "external ref correlating the conversation across restarts")
	fs.StringVar(&f.db, "db", paths.Get(paths.DB), "postgres url for conversation persistence (empty: in-memory)")
	fs.StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: <XDG_CACHE_HOME>/rafiki/spill/<ref>)")
	fs.StringVar(&f.name, "name", "", "session name reported through get_state")
	fs.IntVar(&f.maxOutputTokens, "max-output-tokens", 0, "per-turn output token cap sent to upstream (0 = default 16384)")
	fs.BoolVar(&f.recordRequests, "record-requests", false, "Record raw LLM API requests and responses for debugging")
	fs.StringVar(&f.bashRTK, "bash-rtk", "", "route bash commands through rtk for output compression: auto, on, or off (overrides $RAFIKI_BASH_RTK)")
	fs.BoolVar(&f.toolsWeb, "tools-web", false, "enable the webfetch/websearch tools (overrides $RAFIKI_TOOLS_WEB; default off; disable with --tools-web=false)")
	// --fake-turns was read by runAgent (EngineConfig.FakeTurns) and listed
	// in docs/agent-cli.md, but never registered here — so the field was
	// permanently empty and passing the flag was a hard parse error rather
	// than a silent no-op. That is what broke
	// TestIntegration_AgentKind_AbortPreservesProcess, which spawns an agent
	// child with --fake-turns. Same defect class as the --record-requests
	// bug, and the reason this flag set is shared with printAgentUsage in
	// the first place: so the documented flags cannot drift from the parsed
	// ones. A flag that skips it defeats the whole arrangement.
	fs.StringVar(&f.fakeTurns, "fake-turns", "", "replay a recorded turn file instead of calling upstream (testing)")

	return fs
}

// resolveMCPConfig determines the effective .mcp.json path for an agent
// child, in precedence order: an explicit --mcp-config value always wins;
// otherwise <cwd>/.mcp.json if it exists (a project's own MCP servers must
// win over a machine-wide default); otherwise paths.GlobalMCPConfig() (Task
// A2's $RAFIKI_MCP_CONFIG-or-<ConfigDir>/mcp.json fallback, unused until this
// wiring).
//
// The returned path is not guaranteed to exist when it comes from the cwd or
// global fallback — runAgent's own os.Stat decides whether to load it or
// silently skip it, mirroring the pre-existing default behaviour. Only an
// explicit flagValue is exempt from that "may not exist" contract in the
// sense that its absence is a hard startup error, not a skip — but that
// distinction is enforced by the caller (runAgent), not here: this function
// stays pure aside from the one os.Stat needed to test cwd-file existence.
func resolveMCPConfig(flagValue, cwd string) string {
	if flagValue != "" {
		return flagValue
	}
	cwdCfg := filepath.Join(cwd, ".mcp.json")
	if _, err := os.Stat(cwdCfg); err == nil {
		return cwdCfg
	}
	return paths.GlobalMCPConfig()
}

// resolveLSPConfig determines the effective lsp.json path, mirroring the
// resolveMCPConfig precedence order: explicit --lsp-config wins; otherwise
// <cwd>/.lsp.json if present; otherwise $RAFIKI_LSP_CONFIG or <ConfigDir>/lsp.json.
func resolveLSPConfig(flagValue, cwd string) string {
	if flagValue != "" {
		return flagValue
	}
	cwdCfg := filepath.Join(cwd, ".lsp.json")
	if _, err := os.Stat(cwdCfg); err == nil {
		return cwdCfg
	}
	return paths.GlobalLSPConfig()
}
