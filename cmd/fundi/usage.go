package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"git.graveland.dev/brent/fundi/internal/paths"
)

// isHelpArg reports whether arg is a request for usage. Consulted by main
// BEFORE any daemon setup: the daemon takes no flags, so `fundi -h` used to fall
// through into startup and fail on the controller socket instead of printing
// anything. Only the conventional spellings count — treating an unrecognised
// argument as help would silently refuse to start the daemon.
func isHelpArg(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	}
	return false
}

// printRootUsage documents both process modes. fundi is one binary with two
// entry points — the controller daemon (no flags) and `fundi agent`, a single
// agent child speaking pi's rpc protocol on stdio — so usage that mentioned only
// one of them would hide the other entirely.
func printRootUsage(w io.Writer) {
	fmt.Fprint(w, `fundi — coding-agent controller daemon and native agent runtime.

Usage:
  fundi                 Run the controller daemon (takes no flags).
  fundi agent [flags]   Run one agent child on stdio (pi rpc protocol).
                        Spawned by the daemon; see 'fundi agent -h'.
  fundi -h | --help     Show this help.

The daemon listens on a unix socket and stores its state under the XDG base
directories (override with the standard XDG_* variables):
`)
	fmt.Fprintf(w, "  socket   %s\n", paths.SocketPath())
	fmt.Fprintf(w, "  records  %s\n", paths.RecordsDir())
	fmt.Fprintf(w, "  logs     %s\n", paths.LogsDir())
	fmt.Fprint(w, `
$PI_CONTROLLER_SOCKET overrides the socket path for both the daemon's clients
and any child it spawns.
`)
}

// printAgentUsage writes `fundi agent` usage. It exists because
// parseAgentFlags points its FlagSet at io.Discard so runAgent can report parse
// errors itself — which also swallowed the -h output, making `fundi agent -h`
// exit 0 having printed nothing.
func printAgentUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: fundi agent [flags]

Runs a single agent child speaking pi's rpc protocol on stdio, in place of
Claude Code. Normally spawned by the fundi daemon rather than invoked directly.

Flags:
`)
	var f agentFlags
	fs := newAgentFlagSet(&f)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// newAgentFlagSet registers `fundi agent`'s flags. Shared by parseAgentFlags and
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
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "path to .mcp.json (default: <cwd>/.mcp.json if present)")
	fs.StringVar(&f.ref, "ref", envOr("PI_CONTROLLER_CHILD_ID"), "external ref correlating the conversation across restarts")
	fs.StringVar(&f.db, "db", envOr("FUNDI_AGENT_DB"), "postgres url for conversation persistence (empty: in-memory)")
	fs.StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: os.TempDir()/fundi-spill-<ref>)")
	fs.StringVar(&f.name, "name", "", "session name reported through get_state")
	fs.StringVar(&f.fakeTurns, "fake-turns", "", "hidden test seam: path to a LoadFakeSender scripted-turns file")

	return fs
}

// envOr reads an environment variable, used for the flag defaults that fall
// back to the daemon-provided environment.
func envOr(key string) string { return os.Getenv(key) }
