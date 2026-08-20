package tools

import "slices"

// Tier says what a tool needs in order to run at all.
//
// The distinction is not "where the code lives" but "whose machine the effect
// lands on". A daemon tool's effect is a network call or a database row and is
// the same wherever the agent runs; a workspace tool's effect is a file or a
// process on one specific host, and running it on the wrong host is not a
// degraded answer but a wrong one.
//
// The classification is BINARY and TestEveryDeclaredTierIsCarried keeps it
// that way. A third tier for verbs needing the operator's own machine —
// clipboard, editor, browser, file picker — was reserved and deleted unbuilt;
// docs/reference/executor-protocol.md, "Not built: a presence tier", records
// why, because the origin spec for it is persuasive and reads like an
// oversight rather than a decision.
type Tier int

const (
	// TierDaemon runs in the agent's own process and needs no executor.
	// Credentials live here and must not travel below the boundary.
	TierDaemon Tier = iota

	// TierWorkspace touches a filesystem or spawns a process, so it needs an
	// executor and must execute there.
	TierWorkspace

	// tierCount bounds the declared tiers so a tier nothing carries is a test
	// failure rather than dead weight. It must stay LAST; a new tier goes
	// above it.
	tierCount
)

// tierByTool is the single classification of every registered tool.
//
// TestEveryBlueprintHasATier makes an omission a test failure rather than a
// silent confinement hole: `ls` shipped for months outside the routing list
// and therefore listed directories on the daemon's filesystem whenever an
// executor was configured, and nothing failed.
var tierByTool = map[string]Tier{
	// Workspace — filesystem and processes.
	"read":  TierWorkspace,
	"write": TierWorkspace,
	"edit":  TierWorkspace,
	"glob":  TierWorkspace,
	"grep":  TierWorkspace,
	"ls":    TierWorkspace,
	"bash":  TierWorkspace,

	// Workspace — background jobs. Parent-side tools whose implementation is
	// an RPC, so they are routed but never present in the executor's own
	// registry.
	"bash_start":  TierWorkspace,
	"bash_output": TierWorkspace,
	"bash_kill":   TierWorkspace,

	// Workspace — language servers. Classified here because they read (and in
	// lsp_rename's case write) the workspace's files.
	"lsp_call_hierarchy": TierWorkspace,
	"lsp_definition":     TierWorkspace,
	"lsp_diagnostics":    TierWorkspace,
	"lsp_references":     TierWorkspace,
	"lsp_rename":         TierWorkspace,
	"lsp_restart":        TierWorkspace,
	"lsp_symbols":        TierWorkspace,

	// Daemon — network, database, and the agent tree.
	"webfetch":     TierDaemon,
	"websearch":    TierDaemon,
	"task_add":     TierDaemon,
	"task_update":  TierDaemon,
	"task_drop":    TierDaemon,
	"task_list":    TierDaemon,
	"agent_spawn":  TierDaemon,
	"agent_list":   TierDaemon,
	"agent_view":   TierDaemon,
	"agent_send":   TierDaemon,
	"agent_kill":   TierDaemon,
	"agent_models": TierDaemon,

	// Daemon — `skill` loads from paths.SkillsDirs() as well as from the
	// project, so it survives with no executor and only loses its
	// project-local entries. Skill loading is not a workspace capability;
	// skill discovery in the project is.
	"skill": TierDaemon,

	// Daemon — annotates the executor's own database row.
	"executor_annotate": TierDaemon,
}

// notRoutedYet names workspace tools the parent does not forward to an executor.
//
// The three background-job verbs are permanent members: they are parent-side
// tools implemented as RPCs, so proxying them would dispatch bash_start into
// the executor's registry, which does not contain it.
var notRoutedYet = map[string]bool{
	"bash_start":  true,
	"bash_output": true,
	"bash_kill":   true,
}

// TierOf reports a tool's tier. ok is false for a name no blueprint registers.
func TierOf(name string) (Tier, bool) {
	t, ok := tierByTool[name]
	return t, ok
}

// WorkspaceTools returns every tool requiring an executor, sorted.
func WorkspaceTools() []string {
	return namesInTier(TierWorkspace)
}

// ExecutorLocalTools returns the tool names an executor process actually
// serves in its own registry.
func ExecutorLocalTools() []string {
	var out []string
	for _, name := range WorkspaceTools() {
		if notRoutedYet[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// RoutedToExecutor returns the tool names the PARENT dispatches to an executor
// when one is configured. It is ExecutorLocalTools plus the background-job
// verbs, which are parent-side RPCs and never reach the executor's registry.
func RoutedToExecutor() []string {
	out := append(ExecutorLocalTools(), "bash_start", "bash_output", "bash_kill")
	slices.Sort(out)
	return out
}

func namesInTier(want Tier) []string {
	var out []string
	for name, tier := range tierByTool {
		if tier == want {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
