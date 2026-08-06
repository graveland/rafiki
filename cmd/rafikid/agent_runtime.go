package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/inproc"
	"go.graveland.dev/rafiki/pkg/protocol"
)

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
func (c *Controller) agentRunner(req protocol.SpawnRequest, childID string, autoResume bool) (child.Runner, error) {
	if req.Kind != protocol.KindFundi {
		return nil, nil
	}
	ro, err := c.agentRuntimeOptions(req, childID, autoResume)
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
func (c *Controller) agentRuntimeOptions(req protocol.SpawnRequest, childID string, autoResume bool) (fundi.RuntimeOptions, error) {
	if agentSpawnHasExplicitDB(req.ExtraArgs) {
		return fundi.RuntimeOptions{}, errors.New("--db is not supported for an in-process agent child: the daemon's shared database pool is always used instead; drop --db from ExtraArgs")
	}

	argv := appendDaemonRef(buildAgentArgv(req, childID, c.stateDir), childID)
	f, err := parseAgentFlags(argv[1:]) // argv[0] is the "fundi" subcommand
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent flags: %w", err)
	}
	ro, err := f.toRuntimeOptions(req.Cwd, c.pool)
	if err != nil {
		return fundi.RuntimeOptions{}, fmt.Errorf("agent runtime options: %w", err)
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
	// API keys are deliberately NOT forwarded through os.Setenv — they are
	// extracted into the RuntimeOptions named fields below so the runtime
	// reads them directly without exposing them to subprocesses. Daemon env <
	// forwarded env < explicit key.
	ro.Env = make(map[string]string, len(req.Env))
	for k, v := range req.Env {
		switch k {
		case "ANTHROPIC_API_KEY":
			ro.AnthropicAPIKey = v
		case "OPENROUTER_API_KEY":
			ro.OpenRouterAPIKey = v
		default:
			ro.Env[k] = v
		}
	}

	// An in-process child inherits no environment at all, so req.APIKey - the
	// channel the subprocess path uses via buildEnv's
	// ANTHROPIC_API_KEY/OPENROUTER_API_KEY injection - must be overlaid onto
	// RuntimeOptions directly here, or it is silently dropped and the child
	// either fails (no daemon-env key) or runs on the daemon's own key instead
	// of the caller's. Same prefix rule buildEnv uses, but keyed off f.model
	// (the flag as actually parsed, i.e. after any ExtraArgs --model override)
	// rather than req.Model, so an override still routes the key to the right
	// field. Only overlay when the caller actually supplied a key, so an unset
	// req.APIKey leaves the value set above (forwarded env, or the daemon's
	// own environment) in place, matching the subprocess path's behavior when
	// buildEnv's req.APIKey != "" guard is false. This is deliberately the
	// LAST overlay applied: an explicit key is the caller's most specific,
	// most deliberate instruction and must win over a merely-forwarded one.
	if req.APIKey != "" {
		if strings.HasPrefix(f.model, "anthropic/") {
			ro.AnthropicAPIKey = req.APIKey
		} else {
			ro.OpenRouterAPIKey = req.APIKey
		}
	}
	ro.AutoResume = autoResume
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
func (f agentFlags) toRuntimeOptions(cwd string, pool *pgxpool.Pool) (fundi.RuntimeOptions, error) {
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
		SkillsDirs:           assembleSkillDirs(cwd, f.skillsDir),
		Skills:               f.skills,
		NoSkills:             f.noSkills,
		NoContextFiles:       f.noContextFiles,
		MCPConfig:            mcpPath,
		FakeTurns:            f.fakeTurns,
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		OpenRouterAPIKey:     os.Getenv("OPENROUTER_API_KEY"),
		Pool:                 pool,
	}, nil
}
