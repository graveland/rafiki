package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
)

// isHelpArg reports whether arg is a request for usage. Consulted by main
// BEFORE any daemon setup: the daemon takes no flags, so `fundid -h` used to
// fall through into startup and fail on the controller socket instead of
// printing anything. Only the conventional spellings count — treating an
// unrecognised argument as help would silently refuse to start the daemon.
func isHelpArg(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	}
	return false
}

// printRootUsage documents both process modes. fundid is one binary with two
// entry points — the controller daemon (no flags) and `fundid fundi`, a single
// agent child speaking pi's rpc protocol on stdio — so usage that mentioned only
// one of them would hide the other entirely.
func printRootUsage(w io.Writer) {
	fmt.Fprint(w, `fundid — coding-agent controller daemon and native agent runtime.

Usage:
  fundid [flags]          Run the controller daemon.
  fundid fundi [flags]    Run one agent child on stdio (pi rpc protocol).
                          Spawned by the daemon; see 'fundid fundi -h'.
  fundid -h | --help      Show this help.

Daemon flags:
  -config string   config file (named client tokens, openai routes, default model)
  -listen string   proxy face listen address (overrides FUNDI_PROXY_LISTEN)
  -db string       postgres DSN (overrides FUNDI_AGENT_DB)
  -dev             dev mode: auto-migrate the schema, accept the token "dev"

The command-line client is a separate binary, `+"`fundi`"+`.

The daemon listens on a unix socket and stores its state under the XDG base
directories (override with the standard XDG_* variables). These are this
process's own resolved paths — a client (fundi, or a launchd/systemd unit
with a different environment) may resolve differently if its HOME or XDG_*
variables disagree with the daemon's:
`)
	fmt.Fprintf(w, "  %-12s %s\n", "socket", paths.SocketPath())
	fmt.Fprintf(w, "  %-12s %s\n", "records", paths.RecordsDir())
	fmt.Fprintf(w, "  %-12s %s\n", "logs", paths.LogsDir())
	fmt.Fprintf(w, "  %-12s %s\n", "instructions", paths.InstructionsFile())
	for i, d := range paths.SkillsDirs() {
		label := ""
		if i == 0 {
			label = "skills"
		}
		fmt.Fprintf(w, "  %-12s %s\n", label, d)
	}
	fmt.Fprintf(w, "  %-12s %s\n", "presets", paths.PresetsFile())
	fmt.Fprintf(w, "  %-12s %s\n", "mcp", paths.GlobalMCPConfig())
	fmt.Fprint(w, `
$FUNDI_SOCKET overrides the socket path for both the daemon's clients
and any child it spawns. $FUNDI_INSTRUCTIONS, $FUNDI_SKILLS_DIRS, and
$FUNDI_MCP_CONFIG override the instructions/skills/mcp paths above.
`)
}

// printAgentUsage writes `fundid fundi` usage. It exists because
// parseAgentFlags points its FlagSet at io.Discard so runAgent can report parse
// errors itself — which also swallowed the -h output, making `fundid fundi -h`
// exit 0 having printed nothing.
func printAgentUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: fundid fundi [flags]

Runs a single agent child speaking pi's rpc protocol on stdio, in place of
Claude Code. Normally spawned by the fundi daemon rather than invoked directly.

Flags:
`)
	var f agentFlags
	fs := newAgentFlagSet(&f)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// newAgentFlagSet registers `fundid fundi`'s flags. Shared by parseAgentFlags and
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
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "path to .mcp.json (default: <cwd>/.mcp.json if present, else $FUNDI_MCP_CONFIG or <ConfigDir>/mcp.json)")
	fs.StringVar(&f.ref, "ref", paths.Get(paths.ChildID), "external ref correlating the conversation across restarts")
	fs.StringVar(&f.db, "db", paths.Get(paths.AgentDB), "postgres url for conversation persistence (empty: in-memory)")
	fs.StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: <XDG_CACHE_HOME>/fundi/spill/<ref>)")
	fs.StringVar(&f.name, "name", "", "session name reported through get_state")
	fs.StringVar(&f.fakeTurns, "fake-turns", "", "hidden test seam: path to a LoadFakeSender scripted-turns file")

	return fs
}

// resolveMCPConfig determines the effective .mcp.json path for an agent
// child, in precedence order: an explicit --mcp-config value always wins;
// otherwise <cwd>/.mcp.json if it exists (a project's own MCP servers must
// win over a machine-wide default); otherwise paths.GlobalMCPConfig() (Task
// A2's $FUNDI_MCP_CONFIG-or-<ConfigDir>/mcp.json fallback, unused until this
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
