package tools

import "slices"

// ServedSet converts a Describe.tools slice into the lookup map the routing
// filter reads. A nil slice stays nil — the distinction between "unknown" and
// "empty" is what keeps a caller with no Describe (the socket path) unfiltered;
// an EMPTY non-nil slice means the machine reported serving nothing, so every
// routed tool is dropped.
//
// The ONE implementation: pkg/fundi and the daemon's MCP face both filter
// through this, so a tool advertised-but-unrunnable on one face cannot be
// served on the other.
func ServedSet(served []string) map[string]bool {
	if served == nil {
		return nil
	}
	out := make(map[string]bool, len(served))
	for _, name := range served {
		out[name] = true
	}
	return out
}

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

	// Workspace — Claude Code's own four, translated. Each forwards to the
	// native member of this tier on the executor (Read → read, Bash → bash), so
	// the effect lands on the same machine as every other workspace tool; the
	// tier entry is also what gives them the per-call `executor` argument. They
	// are served only to a claude child in workspace-tools mode — see
	// claude_shaped.go — and never reach an executor's own registry (see
	// notRoutedYet).
	"Read":  TierWorkspace,
	"Write": TierWorkspace,
	"Edit":  TierWorkspace,
	"Bash":  TierWorkspace,

	// Workspace — network AND filesystem. download's effect is a file on a
	// specific host, so it runs where that file lives; the fetch happens
	// there too, confined by that host's own egress (a sandbox with
	// network=none fails the download, which is correct).
	"download": TierWorkspace,

	// Workspace — pymodules. pymodule_run executes a saved pymodule out of
	// the executor's synced cache: it needs the executor's interpreter and
	// corpus, though the run itself never touches workspace files.
	"pymodule_run": TierWorkspace,

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
	"lsp_replace_symbol": TierWorkspace,
	"lsp_restart":        TierWorkspace,
	"lsp_symbols":        TierWorkspace,

	// Daemon — network, database, and the agent tree.
	"webfetch":         TierDaemon,
	"websearch":        TierDaemon,
	"task_add":         TierDaemon,
	"task_update":      TierDaemon,
	"task_drop":        TierDaemon,
	"task_list":        TierDaemon,
	"agent_spawn":      TierDaemon,
	"agent_list":       TierDaemon,
	"agent_view":       TierDaemon,
	"agent_send":       TierDaemon,
	"agent_kill":       TierDaemon,
	"agent_models":     TierDaemon,
	"agent_set_budget": TierDaemon,
	"agent_route":      TierDaemon,
	// The tag verbs read and write the daemon's tag table through the same
	// per-caller binding agent_spawn uses; nothing in the workspace is touched.
	"agent_tag":           TierDaemon,
	"agent_untag":         TierDaemon,
	"agent_tags":          TierDaemon,
	"agent_report":        TierDaemon,
	"agent_result":        TierDaemon,
	"quota_status":        TierDaemon,
	"conversation_search": TierDaemon,
	"conversation_export": TierDaemon,
	"conversation_query":  TierDaemon,

	// Daemon — `skill` loads from paths.SkillsDirs() as well as from the
	// project, so it survives with no executor and only loses its
	// project-local entries. Skill loading is not a workspace capability;
	// skill discovery in the project is.
	"skill": TierDaemon,

	// Daemon — pymodule_put, pymodule_get and pymodule_delete touch the
	// daemon's database, owner-scoped; nothing in the workspace is touched.
	// Saved modules reach executors via the pusher, never via the tool.
	// pymodule_start is daemon-side too: it SPAWNS a script child (the same
	// daemon authority agent_spawn exercises); the child it starts may later
	// be HOSTED on an executor, but the start call itself touches no
	// workspace.
	"pymodule_put":    TierDaemon,
	"pymodule_get":    TierDaemon,
	"pymodule_delete": TierDaemon,
	"pymodule_start":  TierDaemon,

	// Daemon — presets live in the daemon's database; nothing in the
	// workspace is touched.
	"preset_list":   TierDaemon,
	"preset_get":    TierDaemon,
	"preset_put":    TierDaemon,
	"preset_delete": TierDaemon,

	// Daemon — recall reads the daemon's captured conversations and the
	// caller's own memories; nothing in the workspace is touched.
	"recall":         TierDaemon,
	"recall_context": TierDaemon,
	"memory_put":     TierDaemon,
	"memory_get":     TierDaemon,
	"memory_tree":    TierDaemon,
	"memory_delete":  TierDaemon,

	// Daemon — the schedule verbs create and read rows in the daemon's
	// schedule table through the per-child ScheduleManager; nothing in the
	// workspace is touched, and the child binding lives on the daemon.
	"CronCreate":     TierDaemon,
	"CronDelete":     TierDaemon,
	"CronList":       TierDaemon,
	"ScheduleWakeup": TierDaemon,

	// Daemon — annotates the executor's own database row.
	"executor_annotate": TierDaemon,

	// Daemon — the install verbs drive the daemon's install orchestrator: the
	// ssh is run by one of the CALLER's executors and the effect lands on a
	// machine described by an executor row. Nothing in the caller's own
	// workspace is touched, so the verb answers the same wherever the agent
	// runs — which is what makes it usable from a child that could not reach
	// the target itself.
	"executor_install":   TierDaemon,
	"executor_uninstall": TierDaemon,
	// Daemon — a read of the executor store plus the pool's live health view.
	// Nothing in the caller's workspace is touched, and the answer is the same
	// wherever the agent runs, which is what lets a machine-hosted agent check
	// on a machine it provisioned from elsewhere.
	"executor_list": TierDaemon,

	// Daemon — the sandbox verbs touch the daemon's sandbox table and the
	// executor pool, never the caller's filesystem: the container they name
	// runs on the launcher's machine, so the effect is the same wherever the
	// agent runs. sandbox_sync and sandbox_sync_repo are daemon-brokered
	// transfers BETWEEN executors: the daemon relays them, so the verb's own
	// effect is a network call on the daemon, not a file on the agent's host.
	"sandbox_create":    TierDaemon,
	"sandbox_list":      TierDaemon,
	"sandbox_remove":    TierDaemon,
	"sandbox_sync":      TierDaemon,
	"sandbox_sync_repo": TierDaemon,
}

// notRoutedYet names workspace tools the parent does not forward to an executor.
//
// Two families are permanent members:
//
// The three background-job verbs are parent-side tools implemented as RPCs, so
// proxying them would dispatch bash_start into the executor's registry, which
// does not contain it.
//
// The Claude-shaped four (Read/Write/Edit/Bash) are daemon-side TRANSLATORS: a
// model calling Bash reaches the executor through `bash`, and `Bash` itself
// exists nowhere but in the daemon's registry for a claude child. Proxying one
// would ask the executor's registry for a tool named "Bash", which no
// MaterializeOnly over ExecutorLocalTools could ever put there.
var notRoutedYet = map[string]bool{
	"bash_start":  true,
	"bash_output": true,
	"bash_kill":   true,

	"Read":  true,
	"Write": true,
	"Edit":  true,
	"Bash":  true,
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

// RoutedToExecutor returns the tool names the PARENT proxies to an executor
// when one is configured: ExecutorLocalTools plus the background-job verbs,
// which are parent-side RPCs and never reach the executor's registry.
//
// A tool ABSENT from this list is not necessarily unrouted — the Claude-shaped
// four call the executor themselves from the daemon (notRoutedYet), so they are
// machine-local work the parent dispatches without proxying the call.
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
