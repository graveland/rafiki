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
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/inproc"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/skills"
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
	senders, err := providerSenders(ro.Providers, c.execPoolConn)
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent runtime options: %w", err)
	}
	ro.ProviderSenders = senders

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
	exec, err := c.resolveExecutor(req, ownerName)
	if err != nil {
		return fundi.RuntimeOptions{}, err
	}

	// Provision workspace when using a label-selected executor.
	var execID string
	if req.ExecutorSelector != "" && c.execPool != nil {
		id, _, selErr := c.selectExecutorID(req, ownerName)
		if selErr == nil {
			execID = id
			wsID, wsInfo, wsExec, wsErr := c.provisionWorkspace(context.Background(), req, execID)
			if wsErr != nil {
				return fundi.RuntimeOptions{}, fmt.Errorf("workspace: %w", wsErr)
			}
			exec = wsExec

			// The project tier belongs to the workspace, which lives on this
			// executor. Fetch it and hand it down. The pointer is set
			// unconditionally for an executor-backed child — even to the empty
			// string, the ordinary answer — so resolveContent never falls back to
			// reading the daemon's cwd for a workspace that is not here.
			project, pcErr := fetchProjectContext(context.Background(), exec)
			if pcErr != nil {
				slog.Warn("project context unavailable; agent gets global instructions only",
					"child", childID, "error", pcErr)
			}
			ro.ProjectContext = &project

			// Fetch the project-tier skills from the same executor, and carry
			// them pre-merged so BuildRuntime's resolveContent applies the
			// --skills filter to the combined inventory rather than only to
			// the local tier.
			remote, psErr := fetchProjectSkills(context.Background(), exec)
			if psErr != nil {
				slog.Warn("project skills unavailable",
					"child", childID, "error", psErr)
			}
			ro.RemoteSkills = remote

			// Build a lazy fetcher bound to the same workspace client so the
			// skill tool fetches bodies on the turn the model asks.
			if pf, ok := exec.(projectSkillsFetcher); ok {
				pf := pf // pin for the closure
				ro.RemoteSkillBody = func(ctx context.Context, name string) (string, string, error) {
					return pf.SkillBody(ctx, name)
				}
			}

			// The mode comes from the executor's ROW, carried here on wsInfo.
			// It decides whether losing the executor fails this child or moves
			// it, so it cannot be a literal and it cannot be the executor's own
			// claim about itself.
			mode := "pinned"
			if wsInfo != nil {
				mode = workspaceModeOrPinned(wsInfo.WorkspaceMode)
			}
			c.wsLabelsMu.Lock()
			if c.wsLabels == nil {
				c.wsLabels = make(map[string]workspaceLabels)
			}
			c.wsLabels[childID] = workspaceLabels{workspaceID: wsID, executorID: execID, mode: mode}
			c.wsLabelsMu.Unlock()

			if wsInfo != nil {
				// The worker's system prompt names where it landed. ExecutorName
				// comes from the resolved executor rather than from the row read
				// during provisioning only because chooseExecutor already has the
				// display name in hand.
				if chosen, cErr := c.chooseExecutor(req, ownerName); cErr == nil {
					wsInfo.ExecutorName = chosen.DisplayName
				}
				ro.Workspace = wsInfo
			}
		}
	}

	ro.Executor = exec
	ro.ExecutorTools = c.executorToolsFor(execID)
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
