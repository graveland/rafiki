package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// isHelpArg reports whether arg is a request for usage. Consulted by main
// BEFORE any daemon setup: the daemon takes no flags, so `rafikid -h` used to
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

// isSubcommand reports whether arg names a subcommand that must be dispatched
// before the daemon's own flag parsing. Each is a separate process mode.
func isSubcommand(arg string) bool {
	switch arg {
	case protocol.KindFundi, "agent", "migrate":
		return true
	}
	return false
}

// printRootUsage documents every process mode. rafikid is one binary with
// several entry points — the controller daemon (no flags), `rafikid fundi` (a
// single agent child speaking pi's rpc protocol on stdio), `rafikid agent` (the
// DSN-backed insights CLI), and `rafikid migrate` — so usage that mentioned
// only one of them would hide the others entirely.
func printRootUsage(w io.Writer) {
	fmt.Fprint(w, `rafikid — coding-agent controller daemon and native agent runtime.

Usage:
  rafikid [flags]          Run the controller daemon.
  rafikid fundi [flags]    Run one agent child on stdio (pi rpc protocol).
                           Spawned by the daemon; see 'rafikid fundi -h'.
  rafikid agent <verb>     DSN-backed insights CLI: stats|search|export|
                           analyze|findings. See 'rafikid agent' with no verb.
  rafikid migrate [flags]  Apply the conversations schema migration chain.
  rafikid -h | --help      Show this help.

Daemon flags:
  -config string   config file (named client tokens, openai routes, default model)
  -listen string   proxy face listen address (overrides RAFIKI_PROXY_LISTEN)
  -db string       postgres DSN (overrides RAFIKI_DB)
  -dev             dev mode: auto-migrate the schema, accept the token "dev"

The command-line client is a separate binary, `+"`rafiki`"+`.

The daemon listens on a unix socket and stores its state under the XDG base
directories (override with the standard XDG_* variables). These are this
process's own resolved paths — a client (rafiki, or a launchd/systemd unit
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
$RAFIKI_SOCKET overrides the socket path for both the daemon's clients
and any child it spawns. $RAFIKI_INSTRUCTIONS, $RAFIKI_SKILLS_DIRS, and
$RAFIKI_MCP_CONFIG override the instructions/skills/mcp paths above.
`)
}

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
	fs.StringVar(&f.ref, "ref", paths.Get(paths.ChildID), "external ref correlating the conversation across restarts")
	fs.StringVar(&f.db, "db", paths.Get(paths.DB), "postgres url for conversation persistence (empty: in-memory)")
	fs.StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: <XDG_CACHE_HOME>/rafiki/spill/<ref>)")
	fs.StringVar(&f.name, "name", "", "session name reported through get_state")
	fs.StringVar(&f.fakeTurns, "fake-turns", "", "hidden test seam: path to a LoadFakeSender scripted-turns file")

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
