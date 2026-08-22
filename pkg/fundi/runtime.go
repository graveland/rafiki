package fundi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/lspadapter"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/tasksdb"
)

// RuntimeOptions is everything BuildRuntime needs to assemble an Engine. It is
// deliberately free of process globals: Cwd is explicit because the daemon's
// working directory is never the child's, and ctx is a parameter because a
// daemon must not install per-child signal handlers.
type RuntimeOptions struct {
	Model                string
	ThinkingBudget       int64
	MaxOutputTokens      int // 0 = default (4096); per-turn output cap sent to upstream
	SystemPromptOverride string
	AppendSystemPrompt   string
	Cwd                  string // must be absolute
	Ref                  string
	Name                 string
	SpillDir             string   // defaults to paths.SpillDir(Ref) when empty
	SkillsDirs           []string // already assembled; see assembleSkillDirs in cmd/rafikid
	Skills               string   // comma-separated allowlist; empty means all
	NoSkills             bool
	NoContextFiles       bool
	// ContextFilesBudget is the approximate token budget context files may
	// occupy; 0 means no cap. Set from a model's declared alias — see
	// cmd/rafikid's resolveModelDefaults — never computed here.
	ContextFilesBudget int
	// ProjectContext, when non-nil, is the project tier of instruction files
	// (CLAUDE.md / AGENTS.md at the git root and at cwd, includes expanded)
	// already fetched from the machine holding the workspace. nil means "no
	// executor is bound — there is no machine to answer this, and the
	// daemon's own filesystem is not a stand-in for it" (resolveContent
	// treats a nil pointer as an empty project tier, never as "read cwd on
	// this machine"); a non-nil pointer, even one to "", means "the executor
	// answered" and must be used verbatim, so an executor-backed child with an
	// empty workspace never silently falls back to the daemon's files.
	ProjectContext *string
	// RemoteSkills, when non-nil, is the project-tier skills already fetched
	// from the child's executor. They are merged into the local discovery result
	// with project shadowing user. nil means no executor — all skills are local.
	RemoteSkills []skills.SkillMeta
	// RemoteSkillBody fetches a project skill's body from the child's executor.
	// nil when the child has no executor, in which case no SkillMeta carries Remote.
	RemoteSkillBody func(ctx context.Context, name string) (body, dir string, err error)
	MCPConfig       string // absolute path, or empty to skip MCP entirely
	LSPConfig       string // absolute path, or empty to skip LSP entirely
	FakeTurns       string
	Providers       *providers.Set
	APIKeyOverride  string

	// ProviderSenders overrides the sender for specific providers, keyed by
	// provider name. The daemon populates this for every provider with a
	// via_executor table — it has already resolved the executor and built
	// the relay transport (relayTransport, cmd/rafikid) by the time
	// BuildRuntime runs, because doing that resolution here would require
	// pkg/fundi to know about the executor pool and the label selector
	// matcher. nil (the common case: no via_executor providers configured)
	// changes nothing.
	ProviderSenders map[string]llm.Sender

	// Pool is the shared database pool. A nil Pool means an in-memory
	// conversation. BuildRuntime never opens a pool itself, so a unit test does
	// not need postgres.
	Pool *pgxpool.Pool

	// AutoResume asks the engine to call agentloop.Resume before accepting
	// any inbound prompts — see EngineConfig.AutoResume.
	AutoResume bool

	// Env carries per-child environment variables forwarded from the caller's
	// shell (via `rafiki create --forward-env` / SpawnRequest.Env). BuildRuntime
	// sets each via os.Setenv before constructing the engine, so tools that
	// spawn subprocesses (bash, MCP) inherit them. Keys that the runtime itself
	// reads (AnthropicAPIKey, OpenRouterAPIKey) are extracted into the named
	// fields above rather than relying on the process environment; they still
	// appear in Env as well so subprocesses can see them.
	Env map[string]string

	// OnFatal is the owner's hook for ending this child when a turn panics. It
	// is passed straight through to EngineConfig.OnFatal, whose doc comment
	// carries the contract. Nil is legal and means "log it and stop taking
	// turns" — correct for the standalone `rafikid fundi` process, wrong for
	// the daemon, which supplies one (inproc.Runner) so a panicked
	// conversation becomes an ordinary child exit rather than a wedged child.
	OnFatal func(error)

	// RawTrace, when non-nil, enables raw LLM API request/response capture to
	// the debug raw_http_request hypertable. Created at daemon startup when
	// RAFIKI_RECORD_REQUESTS=1. Nil disables capture.
	RawTrace *rawtrace.RawTraceStore

	// Agents, when non-nil, gives this child the agent_* tools. Supplied by
	// the daemon as a per-child adapter over the Controller; nil for the
	// standalone `rafikid fundi` process, which has no controller and so no
	// subtree to steer.
	Agents tools.AgentSpawner

	// Executor, when non-nil, runs the filesystem and shell tools in a
	// separate process. nil means no workspace tier at all: the workspace
	// tools are not registered, so the child reasons over the daemon tier
	// only.
	Executor tools.ExecutorClient

	// ExecutorTools, when non-nil, is the exact set of tool names the chosen
	// executor serves (from its Describe). nil means unknown and leaves the
	// child's routed set unfiltered.
	ExecutorTools []string

	// InProcessWorkspace marks a process that is its own workspace — the
	// standalone `rafikid fundi` mode. BuildRuntime satisfies the executor
	// rule with an in-process ExecutorClient rather than an exemption, so
	// there is one rule for every process.
	InProcessWorkspace bool

	// Workspace, when non-nil and Isolation != "none", appends a per-child
	// machine block to the system prompt. Resolved by the daemon at spawn.
	Workspace *WorkspaceInfo

	// RTK selects the bash tool's rtk mode: "auto", "on", or "off".
	// Empty means auto. See tools.ParseRTKMode.
	RTK string

	// ToolsWeb enables the fundi webfetch and websearch tools when true.
	// Default false: fundi runs unattended and may have no egress.
	ToolsWeb bool

	// NoLSP suppresses all LSP integration — no config files are read and
	// no auto-detection runs. Set to opt out of the default auto-detect
	// behaviour: without an lsp.json and with NoLSP false (the zero value),
	// BuildRuntime scans PATH for well-known language servers.
	NoLSP bool
}

// resolveContent loads the cwd-relative context files and discovers skills.
// Split out of BuildRuntime so a test can assert both were resolved from
// opts.Cwd and not from the process working directory — the regression this
// function's caller exists to prevent, and one that is otherwise invisible
// because LoadContextFiles skips absent files silently rather than erroring.
func resolveContent(opts RuntimeOptions) (contextFiles string, discovered []skills.SkillMeta, err error) {
	if !opts.NoContextFiles {
		// A nil ProjectContext means no executor is bound for this child —
		// there is no machine to answer "what's the project tier", and the
		// daemon's own filesystem is never a stand-in for it (see the field
		// doc). Treat that as an empty project tier rather than falling back
		// to loadContextFiles' own "read opts.Cwd on this machine" behavior,
		// which is only correct for a caller that IS the workspace machine
		// (e.g. the executor answering its own ProjectContext RPC).
		projectTier := opts.ProjectContext
		if projectTier == nil {
			empty := ""
			projectTier = &empty
		}
		contextFiles, err = loadContextFiles(opts.Cwd, projectTier, opts.ContextFilesBudget)
		if err != nil {
			return "", nil, fmt.Errorf("runtime: load context files: %w", err)
		}
	}

	// NOTE: the local is `discovered`, not `skills` — a variable named `skills`
	// would shadow the imported package of the same name.
	if !opts.NoSkills {
		var only []string
		if opts.Skills != "" {
			only = strings.Split(opts.Skills, ",")
		}
		discovered, err = skills.DiscoverSkills(opts.SkillsDirs, only)
		if err != nil {
			return "", nil, fmt.Errorf("runtime: discover skills: %w", err)
		}
		// The project tier was fetched from the executor and never went through
		// DiscoverSkills, so the --skills filter must be applied here too.
		if len(opts.RemoteSkills) > 0 {
			filtered := make([]skills.SkillMeta, 0, len(opts.RemoteSkills))
			for _, s := range opts.RemoteSkills {
				if only == nil {
					filtered = append(filtered, s)
				} else {
					for _, name := range only {
						if s.Name == name {
							filtered = append(filtered, s)
							break
						}
					}
				}
			}
			// MergeSkills puts project after local so project shadows user.
			discovered = MergeSkills(discovered, filtered)
		}
	}

	return contextFiles, discovered, nil
}

// MergeSkills combines the daemon's user and system skills with the project
// skills discovered on the executor.
//
// Project shadows user on a name collision, matching DiscoverSkills' own
// documented rule that later directories win — a project that ships a `deploy`
// skill means its own, not the operator's.
func MergeSkills(local, project []skills.SkillMeta) []skills.SkillMeta {
	byName := make(map[string]skills.SkillMeta, len(local)+len(project))
	order := make([]string, 0, len(local)+len(project))
	for _, s := range append(append([]skills.SkillMeta{}, local...), project...) {
		if _, seen := byName[s.Name]; !seen {
			order = append(order, s.Name)
		}
		byName[s.Name] = s
	}
	sort.Strings(order)
	out := make([]skills.SkillMeta, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// wantsLSP reports whether BuildRuntime should construct a language-server
// manager. Only a process that is its own workspace can reach the lsp_* tools
// through opts.LSP; everywhere else they are proxied to the executor or not
// registered at all.
func wantsLSP(opts RuntimeOptions) bool {
	return opts.InProcessWorkspace
}

// checkRipgrep verifies the ripgrep dependency. It does its own lookup
// rather than consulting tools.RipgrepAvailable so the result is not
// cached across a PATH change (which is what the test exercises).
func checkRipgrep() error {
	if _, err := exec.LookPath("rg"); err != nil {
		return fmt.Errorf("runtime: ripgrep (rg) is required but was not found on PATH; " +
			"install it with `apt-get install ripgrep` or `brew install ripgrep`")
	}
	return nil
}

// checkRTK enforces the one thing that distinguishes RTKOn from RTKAuto.
//
// Both modes use rtk when it is present and fall through to plain bash when
// it is not; the whole point of `on` is that an operator who sized a
// deployment's context budget around rtk compression gets told at startup
// when the binary is missing, rather than silently burning 5-10x the tokens
// on git/docker/kubectl output for the life of the process. Without this
// check `on` was byte-identical to `auto` — a documented flag value that
// did nothing.
//
// The per-call path in rtkRewrite deliberately keeps failing open even in
// this mode: a compression layer must never be able to fail a command. The
// guarantee belongs at startup, which is the only place it can be stated
// once instead of N times per turn.
//
// Like checkRipgrep, this does its own lookup rather than consulting the
// cached probe so the result is not fixed across a PATH change.
func checkRTK(mode tools.RTKMode) error {
	if mode != tools.RTKOn {
		return nil
	}
	if _, err := exec.LookPath("rtk"); err != nil {
		return fmt.Errorf("runtime: --bash-rtk=on (or RAFIKI_BASH_RTK=on) requires rtk, " +
			"but it was not found on PATH; install it, or use `auto` to fall back to " +
			"plain bash when rtk is absent")
	}
	return nil
}

// executorToolSet converts a Describe.tools slice into the lookup map the
// registry's routing filter uses. A nil slice stays nil — the distinction
// between "unknown" and "empty" is what keeps the socket path unfiltered.
func executorToolSet(served []string) map[string]bool {
	if served == nil {
		return nil
	}
	out := make(map[string]bool, len(served))
	for _, name := range served {
		out[name] = true
	}
	return out
}

// BuildRuntime assembles the tool registry, skills, MCP connections, and the
// Engine. The returned shutdown func releases MCP connections and engine
// resources; call it exactly once.
//
// MCPConfig is required to exist when non-empty. The "silently skip an absent
// defaulted <cwd>/.mcp.json" rule lives in the caller, which passes an empty
// MCPConfig in that case — keeping the policy decision where the default is
// computed rather than duplicating it here.
func BuildRuntime(ctx context.Context, fe *Frontend, opts RuntimeOptions) (*Engine, func(), error) {
	if !filepath.IsAbs(opts.Cwd) {
		return nil, nil, fmt.Errorf("runtime: cwd must be absolute: %q", opts.Cwd)
	}

	if err := checkRipgrep(); err != nil {
		return nil, nil, err
	}
	if err := checkRTK(tools.ParseRTKMode(opts.RTK)); err != nil {
		return nil, nil, err
	}

	// Forward the caller's environment into the daemon process so subprocesses
	// spawned by tools (bash, MCP) inherit it. os.Setenv is process-global, but
	// the caller's shell env vars are identical across all of that caller's
	// children — and the only alternative (per-Command.Env on every exec) would
	// require threading this map through every tool, which is more invasive and
	// error-prone.
	for k, v := range opts.Env {
		os.Setenv(k, v)
	}

	spillDir := opts.SpillDir
	if spillDir == "" {
		ref := opts.Ref
		if ref == "" {
			ref = "standalone"
		}
		spillDir = paths.SpillDir(ref)
	}
	outputPolicy := tools.OutputPolicy{SpillDir: spillDir}

	contextFiles, discovered, err := resolveContent(opts)
	if err != nil {
		return nil, nil, err
	}

	// One FileTracker for every tool, including the LSP mutation path: the
	// read-before-edit invariant and the per-path locks are only meaningful
	// if all writers share the same instance.
	fileTracker := tools.NewFileTracker()

	lspShutdown := func() {}
	var lspClient tools.LSPClient
	var lspNotifier tools.FileChangeNotifier

	// Only a process that is its own workspace can reach these. With a remote
	// executor the lsp_* tools are proxied and executorProxy.Execute never
	// touches opts.LSP; with no executor they are not registered at all. The
	// cost of building it anyway is an exec.LookPath per configured server, per
	// child, for a manager nothing can call.
	if wantsLSP(opts) {
		var lspCfg lsp.Config
		lspCfg.Servers = make(map[string]lsp.ServerConfig)
		if opts.LSPConfig != "" {
			if _, err := os.Stat(opts.LSPConfig); err != nil {
				return nil, nil, fmt.Errorf("runtime: lsp config %s: %w", opts.LSPConfig, err)
			}
			var err error
			lspCfg, err = lsp.LoadConfig(opts.LSPConfig)
			if err != nil {
				return nil, nil, fmt.Errorf("runtime: load lsp config %s: %w", opts.LSPConfig, err)
			}
		} else if !opts.NoLSP {
			lspCfg = lsp.AutoDetect()
			if len(lspCfg.Servers) > 0 {
				names := make([]string, 0, len(lspCfg.Servers))
				for n := range lspCfg.Servers {
					names = append(names, n)
				}
				slog.Info("runtime: auto-detected language servers on PATH", "servers", names)
			}
		}

		if len(lspCfg.Servers) > 0 {
			lspMgr := lsp.NewManager(lspCfg, opts.Cwd)
			// A config naming a server that is not installed (or an empty
			// {"servers":{}}) would otherwise put eight tools in tools[] that can
			// only fail with `executable file not found in $PATH`, burning a turn
			// every time the model reaches for one.
			if lspMgr.HasInstalledServer() {
				lspShutdown = func() { lspMgr.Shutdown(context.Background()) }
				lspClient = lspadapter.New(lspMgr, fileTracker)
				lspNotifier = lspMgr
			} else {
				slog.Warn("runtime: no configured language server found on PATH; lsp tools disabled",
					"config", opts.LSPConfig)
			}
		}
	}

	taskStore := tasks.Store(tasks.NewMemoryStore())
	if opts.Pool != nil {
		taskStore = tasksdb.NewPostgresStore(opts.Pool)
	}

	toolOpts := tools.ToolOpts{
		Cwd:             opts.Cwd,
		FileTracker:     fileTracker,
		OutputPolicy:    outputPolicy,
		Skills:          discovered,
		RTK:             tools.ParseRTKMode(opts.RTK),
		Web:             opts.ToolsWeb,
		LSP:             lspClient,
		FileChanged:     lspNotifier,
		Tasks:           taskStore,
		ChildID:         opts.Ref,
		Agents:          opts.Agents,
		Executor:        opts.Executor,
		ExecutorTools:   executorToolSet(opts.ExecutorTools),
		RemoteSkillBody: opts.RemoteSkillBody,
	}
	// A process that is its own workspace satisfies the executor rule with a
	// real in-process client rather than an exemption. Build it here, where the
	// opts it materializes from are the same ones the main registry uses, so
	// the two can never drift apart.
	if opts.InProcessWorkspace && toolOpts.Executor == nil {
		exec, served := tools.NewInProcessExecutor(toolOpts)
		toolOpts.Executor = exec
		toolOpts.ExecutorTools = served
	}
	registry := tools.DefaultBlueprint.MaterializeAll(toolOpts)

	mcpShutdown := func() {}
	if opts.MCPConfig != "" {
		if _, err := os.Stat(opts.MCPConfig); err != nil {
			return nil, nil, fmt.Errorf("runtime: mcp config %s: %w", opts.MCPConfig, err)
		}
		mcpCfg, err := tools.LoadMCPConfig(opts.MCPConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: load mcp config %s: %w", opts.MCPConfig, err)
		}
		mcpShutdown, err = tools.ConnectMCP(ctx, registry, mcpCfg, outputPolicy)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: connect mcp: %w", err)
		}
	}

	cfg := Config{
		Model:                opts.Model,
		ThinkingBudget:       opts.ThinkingBudget,
		MaxOutputTokens:      opts.MaxOutputTokens,
		SystemPromptOverride: opts.SystemPromptOverride,
		AppendSystemPrompt:   opts.AppendSystemPrompt,
		ContextFiles:         contextFiles,
		SkillsInventory:      skills.SkillsInventory(discovered),
		Cwd:                  opts.Cwd,
		Workspace:            opts.Workspace,
		Ref:                  opts.Ref,
		Name:                 opts.Name,
		FakeTurns:            opts.FakeTurns,
		Providers:            opts.Providers,
		APIKeyOverride:       opts.APIKeyOverride,
		ProviderSenders:      opts.ProviderSenders,
		Pool:                 opts.Pool,
		Tools:                registry,
		AutoResume:           opts.AutoResume,
		OnFatal:              opts.OnFatal,
		RawTrace:             opts.RawTrace,
	}

	eng, engShutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		lspShutdown()
		mcpShutdown()
		return nil, nil, fmt.Errorf("runtime: build engine: %w", err)
	}
	return eng, func() {
		lspShutdown()
		mcpShutdown()
		engShutdown()
	}, nil
}
