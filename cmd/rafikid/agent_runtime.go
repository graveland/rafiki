package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/inproc"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/store"
)

// projectContextFetcher is the one method the daemon needs from an executor
// client beyond tools.ExecutorClient: the project instruction files for the
// workspace that client is scoped to. Declared here, at the consumer, rather
// than widening tools.ExecutorClient — only the workspace-scoped client can
// answer it (it carries its own workspace id), and the tool layer has no use
// for it.
type projectContextFetcher interface {
	ProjectContext(ctx context.Context) (string, error)
}

// fetchProjectContext asks an executor client for its workspace's project tier.
// A client that cannot answer — nil, or one without a workspace (the legacy
// socket path) — yields an empty string and no error: the empty string is still
// a real answer, and the caller passes it down as a non-nil pointer so an
// executor-backed child never falls back to reading the daemon's own cwd.
func fetchProjectContext(ctx context.Context, exec any) (string, error) {
	if pf, ok := exec.(projectContextFetcher); ok {
		return pf.ProjectContext(ctx)
	}
	return "", nil
}

// projectSkillsFetcher is the second optional capability the daemon wants from
// a workspace-scoped executor client. Declared at the consumer, like
// projectContextFetcher, rather than widening tools.ExecutorClient — only a
// client carrying a workspace id can answer it.
type projectSkillsFetcher interface {
	ProjectSkills(ctx context.Context) ([]skills.SkillMeta, error)
	SkillBody(ctx context.Context, name string) (body, dir string, err error)
}

// fetchProjectSkills asks an executor client for its workspace's project-tier
// skills. A client that cannot answer yields nil.
func fetchProjectSkills(ctx context.Context, exec any) ([]skills.SkillMeta, error) {
	if pf, ok := exec.(projectSkillsFetcher); ok {
		return pf.ProjectSkills(ctx)
	}
	return nil, nil
}

// appendDaemonRef appends the authoritative --ref last so it wins over
// req.ExtraArgs under buildAgentArgv's last-flag-wins convention. A subprocess
// child receives its id through the injected RAFIKI_CHILD_ID env var; an
// in-process child inherits no env, so the id must travel in argv. It must not
// be caller-overridable: --ref selects which stored conversation the child
// reattaches, so a spoofed value points one child at another's history.
func appendDaemonRef(argv []string, childID string) []string {
	return append(argv, "--ref", childID)
}

// agentSpawnHasExplicitDB reports whether req.ExtraArgs itself carries an
// explicit --db/-db token (one or two leading dashes: flag.FlagSet accepts
// both, so detection must too, or a single-dash spelling evades it and the
// caller's DSN is silently discarded - exactly the failure mode this check
// exists to eliminate). This is deliberately NOT the same question as "is
// agentFlags.db non-empty": newAgentFlagSet defaults f.db from
// $RAFIKI_DB, read in the daemon's OWN process environment — the exact
// env var main.go already read to open the shared pool passed into
// toRuntimeOptions. So an ordinary daemon deployment with a configured
// database has a non-empty f.db on every single parse, override or not, and
// that default names the very database the shared pool already points at —
// harmless to disregard. Only a caller-supplied override in ExtraArgs asks
// for something the in-process path cannot honor (a second, independent
// pool), so only that case must be rejected. Mirrors agentSpawnHasModel's
// explicit-vs-default detection for --model (which has the identical
// single-dash gap - left alone there, see agentSpawnHasModel's doc comment).
//
// Detection matches the flag TOKEN itself (trimmed of leading dashes),
// deliberately not "and the next token doesn't look like a flag": flag.FlagSet
// happily consumes a dash-prefixed value for a non-bool flag (--db -x sets
// f.db="-x"), so gating on the next token's shape would let that case slip
// past undetected.
func agentSpawnHasExplicitDB(extraArgs []string) bool {
	for _, a := range extraArgs {
		bare := strings.TrimLeft(a, "-")
		if bare == "db" || strings.HasPrefix(bare, "db=") {
			return true
		}
	}
	return false
}

// agentRunner builds the in-process runner for an agent child. Returns a nil
// Runner (and nil error) for every other kind, which leaves SpawnSpec on the
// subprocess path unchanged. autoResume asks the engine to call
// agentloop.Resume before accepting inbound prompts.
func (c *Controller) agentRunner(req protocol.SpawnRequest, childID string, autoResume bool, ownerName string) (child.Runner, error) {
	if req.Kind != protocol.KindFundi {
		return nil, nil
	}
	ro, err := c.agentRuntimeOptions(req, childID, autoResume, ownerName)
	if err != nil {
		return nil, err
	}
	return inproc.New(inproc.Options{
		ChildID: childID,
		Parent:  c.baseCtx,
		Runtime: ro,
	}), nil
}

// agentRuntimeOptions builds the RuntimeOptions for an agent-kind req, and is
// split out of agentRunner so a test can assert on the options directly
// (e.g. that Ref/APIKey ended up right) rather than only on whether a Runner
// came back non-nil, which cannot observe either.
//
// argv is built the same way for every spawn path (buildAgentArgv, the single
// source of per-child agent config) and then parsed back with parseAgentFlags
// — see toRuntimeOptions' doc comment for why this is not a hand-written
// SpawnRequest-to-RuntimeOptions mapping.
func (c *Controller) agentRuntimeOptions(req protocol.SpawnRequest, childID string, autoResume bool, ownerName string) (fundi.RuntimeOptions, error) {
	if agentSpawnHasExplicitDB(req.ExtraArgs) {
		return fundi.RuntimeOptions{}, errors.New("--db is not supported for an in-process agent child: the daemon's shared database pool is always used instead; drop --db from ExtraArgs")
	}

	argv := appendDaemonRef(buildAgentArgv(req, childID, c.stateDir), childID)
	f, err := parseAgentFlags(argv[1:]) // argv[0] is the "fundi" subcommand
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent flags: %w", err)
	}
	ro, err := f.toRuntimeOptions(req.Cwd, c.pool, req.ExecutorSelector != "", c.providers)
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent runtime options: %w", err)
	}
	senders, err := providerSenders(ro.Providers, c.execPoolConn, ro.Model)
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent runtime options: %w", err)
	}
	ro.ProviderSenders = senders

	// The single lease-acquisition site. Every agent path — spawn, resume,
	// startup recovery — reaches BuildEngine, so hooking here is what makes the
	// guard cover all of them. Acquiring in loadChildren alone left every
	// spawned and resumed child unfenced.
	ro.OnConversationResolved = func(lctx context.Context, conversationID string) (store.Lease, error) {
		if c.leases == nil || c.daemonID == "" || conversationID == "" {
			return store.Lease{}, nil
		}
		lease, ok, err := c.leases.Acquire(lctx, conversationID, c.daemonID, leaseTTL)
		if err != nil {
			return store.Lease{}, fmt.Errorf("acquire lease for conversation %s: %w", conversationID, err)
		}
		if !ok {
			return store.Lease{}, fmt.Errorf(
				"conversation %s is driven by another daemon; refusing to start a second engine",
				conversationID)
		}
		c.trackLease(childID, lease)
		c.noteConversationID(childID, conversationID)
		return lease, nil
	}

	// req.Env is buildEnv's second payload for the subprocess path (alongside
	// the API key handled below) - forwarded-caller-environment, default-on
	// via `rafiki create --forward-env` (cmd/rafiki/cmd_create.go). An
	// in-process child cannot receive a per-goroutine environment (Go has
	// none), but it CAN forward the caller's env vars through os.Setenv at
	// engine startup: the caller's shell env is identical across all of that
	// caller's children, and the tools that spawn subprocesses (bash, MCP)
	// inherit them from the daemon process environment.
	//
	// API keys are deliberately NOT forwarded through os.Setenv — the
	// per-spawn key is carried as APIKeyOverride and applied only to the
	// provider the child's model names. Daemon env < forwarded env < explicit key.
	ro.Env = make(map[string]string, len(req.Env))
	for k, v := range req.Env {
		switch k {
		case "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY":
			// Provider keys are NOT forwarded to the child's environment.
			// They are resolved through the provider registry by the daemon,
			// and a per-spawn override travels as req.APIKey below.
		default:
			ro.Env[k] = v
		}
	}

	// An in-process child inherits no environment at all, so req.APIKey - the
	// channel the subprocess path uses via buildEnv's
	// ANTHROPIC_API_KEY/OPENROUTER_API_KEY injection - must be overlaid onto
	// RuntimeOptions directly here, or it is silently dropped and the child
	// either fails (no daemon-env key) or runs on the daemon's own key instead
	// of the caller's. The key lands on the provider the child's MODEL names
	// (Config.APIKeyOverride is keyed off Model), so a forwarded credential
	// can never reach a provider the caller did not address.
	if req.APIKey != "" {
		ro.APIKeyOverride = req.APIKey
	}
	ro.AutoResume = autoResume
	ro.RawTrace = c.rawTrace
	if !req.RecordRequests {
		ro.RawTrace = nil
	}
	// Bound to THIS child. Constructed here rather than stored on Controller
	// because the binding is what makes a self id unspoofable — a shared
	// spawner would have to take one as an argument.
	ro.Agents = newControllerSpawner(c, childID)
	// A child with a selector gets a boundExecutor, ALWAYS non-nil.
	//
	// This bypasses MaterializeAll's `opts.Executor == nil` check, which is a
	// security guard and not a capability check: it is what stops workspace
	// tools running in the daemon process. That is safe only because
	// boundExecutor errors when it cannot resolve and NEVER falls back to
	// in-process execution — see TestBoundExecutorNeverRunsInProcess.
	//
	// Non-nil also means tools[] is identical whether or not an executor is
	// live right now, so a child that starts unbound and acquires an executor
	// later needs no tools[] change and no prompt-cache break.
	var exec tools.ExecutorClient
	if req.ExecutorSelector != "" && c.execPool != nil {
		be := newBoundExecutor(childID, c.binderFor(req, ownerName))
		exec = be

		// Bind eagerly: the project tier and skills below need a live
		// workspace, and a child that CAN bind should start bound.
		//
		// A failure is fatal for an interactive TOP-LEVEL spawn and tolerable
		// for a parented spawn or an auto-resume (e.g. daemon restart recovery
		// before executors reconnect). An agent-spawned or recovered child
		// starts unbound and lazy-binds on its first tool call when its
		// executor connects.
		if _, _, bindErr := be.clientFor(context.Background()); bindErr != nil {
			if req.ParentChildID == "" && !autoResume {
				return fundi.RuntimeOptions{}, bindErr
			}
			slog.Warn("child starts with no executor bound; its workspace tools "+
				"will fail until one matching its selector connects",
				"child", childID, "selector", req.ExecutorSelector, "reason", bindErr)
			c.markUnbound(childID)
		}

		// The project tier belongs to the workspace. The pointer is set
		// UNCONDITIONALLY for any child with a selector — bound or not.
		// pkg/fundi/runtime.go treats a nil pointer as "no executor is bound"
		// and falls back to reading the DAEMON's own cwd, so leaving it nil
		// for an unbound child would silently feed it the daemon's project
		// instructions.
		project, pcErr := fetchProjectContext(context.Background(), exec)
		if pcErr != nil {
			slog.Warn("project context unavailable; agent gets global instructions only",
				"child", childID, "error", pcErr)
			project = ""
		}
		ro.ProjectContext = &project

		remote, psErr := fetchProjectSkills(context.Background(), exec)
		if psErr != nil {
			slog.Warn("project skills unavailable", "child", childID, "error", psErr)
		}
		ro.RemoteSkills = remote

		ro.RemoteSkillBody = func(ctx context.Context, name string) (string, string, error) {
			return be.SkillBody(ctx, name)
		}

		// The system prompt's "Your machine" block comes from the executor's
		// ROW, never from the executor's own account of itself. The block is
		// built even when nothing is bound yet: the system prompt is fixed
		// for the child's lifetime, so omitting it now means never. Prefer
		// the row the child actually bound to; fall back to the first live
		// candidate its selector admits, which is the machine it will land
		// on if one connects.
		//
		// Never from Describe. Isolation and workspace_mode gate where other
		// people's children run and cannot be the executor's own claim.
		var row executors.Executor
		if execID, _, bound := be.Current(); bound {
			row, _ = c.executorRow(execID)
		} else if chosen, cErr := c.chooseExecutorCandidate(req, ownerName); cErr == nil {
			row = chosen
		}
		if row.ID != "" {
			wsInfo := workspaceInfoFromRow(row)
			wsInfo.ExecutorName = row.Labels["machine"]
			ro.Workspace = wsInfo
		}
	}

	ro.Executor = exec
	if be, ok := exec.(*boundExecutor); ok {
		if execID, _, bound := be.Current(); bound {
			ro.ExecutorTools = c.executorToolsFor(execID)
		}
	}
	return ro, nil
}

// toRuntimeOptions converts parsed agent flags into the options an in-process
// engine needs. cwd is explicit because the daemon's working directory is never
// the child's. pool is the daemon's shared pool; a nil pool means an in-memory
// conversation.
//
// This is the only flags-to-options mapping. The daemon builds argv with
// buildAgentArgv and parses it back rather than populating RuntimeOptions
// directly, so ExtraArgs and last-flag-wins behave identically for both
// execution models and no field can be dropped on one path only.
//
// f.db (--db / $RAFIKI_DB) is deliberately NOT read here: the daemon
// owns one shared pool for every in-process child, passed in as pool, and
// f.db's own default is sourced from the same env var the daemon used to open
// that pool in the first place - so silently ignoring it is correct, not lossy.
// agentRuntimeOptions rejects an explicit --db in req.ExtraArgs before this is
// ever reached, so a caller who deliberately asked for a different database
// learns it was refused rather than discovering later their DSN was ignored.
func (f agentFlags) toRuntimeOptions(cwd string, pool *pgxpool.Pool, hasExecutor bool, prov *providers.Set) (fundi.RuntimeOptions, error) {
	thinkingBudget, err := fundi.ThinkingBudgetFor(f.thinking)
	if err != nil {
		return fundi.RuntimeOptions{}, err
	}

	// Mirror runAgent's MCP asymmetry: an explicit --mcp-config that does not
	// exist is an error (BuildRuntime raises it), while an absent defaulted
	// <cwd>/.mcp.json is skipped by passing an empty path.
	mcpPath := resolveMCPConfig(f.mcpConfig, cwd)
	if f.mcpConfig == "" {
		if _, statErr := os.Stat(mcpPath); statErr != nil {
			mcpPath = ""
		}
	}

	lspPath := effectiveLSPConfig(f.lspConfig, cwd)

	effectiveProv := providersOrDefault(prov)
	defaults, _ := resolveModelDefaults(effectiveProv, f.model)
	skillsVal, noSkills := resolveAllowlistOption(f.skills, f.noSkills, defaults.Skills)
	mcpServersVal, noMCP := resolveAllowlistOption(f.mcpServers, f.noMCP, defaults.MCPServers)

	return fundi.RuntimeOptions{
		Model:                f.model,
		ThinkingBudget:       thinkingBudget,
		MaxOutputTokens:      f.maxOutputTokens,
		SystemPromptOverride: f.systemPrompt,
		AppendSystemPrompt:   f.appendSystemPrompt,
		Cwd:                  cwd,
		Ref:                  f.ref,
		Name:                 f.name,
		SpillDir:             f.spillDir,
		SkillsDirs:           assembleSkillDirs(cwd, f.skillsDir, hasExecutor),
		Skills:               skillsVal,
		NoSkills:             noSkills,
		NoContextFiles:       f.noContextFiles,
		ContextFilesBudget:   defaults.ContextFilesTokens,
		MCPConfig:            mcpPath,
		MCPServers:           mcpServersVal,
		NoMCP:                noMCP,
		LSPConfig:            lspPath,
		FakeTurns:            f.fakeTurns,
		Providers:            effectiveProv,
		Pool:                 pool,
		RTK:                  bashRTKValue(f.bashRTK),
		ToolsWeb:             toolsWebValue(f.toolsWeb, f.toolsWebSet),
		NoLSP:                f.noLSP || paths.Get(paths.LSPDisable) == "1",
	}, nil
}
