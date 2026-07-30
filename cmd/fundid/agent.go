package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"git.graveland.dev/brent/fundi/internal/agent"
	"git.graveland.dev/brent/fundi/internal/agent/tools"
	"git.graveland.dev/brent/fundi/internal/paths"
	skillspkg "git.graveland.dev/brent/fundi/internal/skills"
)

// stringSliceFlag implements flag.Value for a repeatable flag (--skills-dir).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// agentFlags is the parsed --model .. --fake-turns flag contract Task 16's
// buildAgentArgv targets verbatim. See parseAgentFlags.
type agentFlags struct {
	model              string
	thinking           string
	systemPrompt       string
	appendSystemPrompt string
	noContextFiles     bool
	skillsDir          stringSliceFlag
	skills             string
	noSkills           bool
	mcpConfig          string
	ref                string
	db                 string
	spillDir           string
	name               string
	fakeTurns          string
}

// parseAgentFlags parses the fundid agent flag set. It is a pure function of
// args plus the environment defaults ($FUNDI_CHILD_ID for --ref,
// $FUNDI_AGENT_DB for --db) so it can be exercised directly by tests without
// a running agent.
func parseAgentFlags(args []string) (agentFlags, error) {
	var f agentFlags
	fs := newAgentFlagSet(&f) // shared with printAgentUsage — see usage.go

	if err := fs.Parse(args); err != nil {
		return agentFlags{}, err
	}

	if f.model == "" || !strings.Contains(f.model, "/") {
		return agentFlags{}, fmt.Errorf(`--model is required and must be provider-qualified, e.g. "anthropic/sonnet-latest" or "deepseek/deepseek-chat"`)
	}
	return f, nil
}

// assembleSkillDirs builds the skill search path, lowest precedence first:
// the configured dirs (paths.SkillsDirs, which is fundi's own config
// location - see that function's doc comment for why it is not
// ~/.claude/skills), then the project's existing <cwd>/.claude/skills (kept
// so a repo that already has one doesn't silently lose its skills), then the
// project's own <cwd>/.fundi/skills (which wins on name collision with
// .claude/skills - skillspkg.DiscoverSkills lets later entries override
// earlier ones), then any --skills-dir flags. Pure so it is testable.
func assembleSkillDirs(cwd string, flagDirs []string) []string {
	dirs := paths.SkillsDirs()
	dirs = append(dirs, filepath.Join(cwd, ".claude", "skills"))
	dirs = append(dirs, filepath.Join(cwd, ".fundi", "skills"))
	return append(dirs, flagDirs...)
}

// runAgent is cmd/fundid's other entry point: `fundid agent ...` runs a single
// agent child speaking pi's rpc protocol on stdio, in place of Claude Code.
// It owns everything internal/agent.Config cannot build itself (env/flag
// parsing, filesystem discovery, and the tool registry - see Config's doc
// comment for why the registry can't be assembled inside internal/agent).
//
// Exit codes: 0 or the reader's clean EOF/graceful-shutdown path, 1 for a
// setup or run error, 2 for a flag-parse error.
func runAgent(args []string) int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	f, err := parseAgentFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The FlagSet's own output is discarded (see newAgentFlagSet), so
			// -h has to print usage explicitly or it exits 0 silently.
			printAgentUsage(os.Stdout)
			return 0
		}
		slog.Error("agent: parse flags", "error", err)
		return 2
	}

	thinkingBudget, err := agent.ThinkingBudgetFor(f.thinking)
	if err != nil {
		slog.Error("agent: invalid --thinking", "error", err)
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("agent: getwd", "error", err)
		return 1
	}

	// ctx is the Engine's BaseCtx: a SIGINT/SIGTERM here cancels any turn in
	// flight, per the engine lifecycle fix in internal/agent/engine.go.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	childID := f.ref
	if childID == "" {
		childID = "standalone"
	}
	spillDir := f.spillDir
	if spillDir == "" {
		spillDir = paths.SpillDir(childID)
	}
	outputPolicy := tools.OutputPolicy{SpillDir: spillDir}

	var contextFiles string
	if !f.noContextFiles {
		contextFiles, err = agent.LoadContextFiles(cwd)
		if err != nil {
			slog.Error("agent: load context files", "error", err)
			return 1
		}
	}

	var skills []skillspkg.SkillMeta
	if !f.noSkills {
		dirs := assembleSkillDirs(cwd, f.skillsDir)

		var only []string
		if f.skills != "" {
			only = strings.Split(f.skills, ",")
		}
		skills, err = skillspkg.DiscoverSkills(dirs, only)
		if err != nil {
			slog.Error("agent: discover skills", "error", err)
			return 1
		}
	}

	registry := tools.NewRegistry()
	tools.RegisterFileTools(registry, tools.NewFileTracker())
	tools.RegisterBash(registry, outputPolicy, cwd)
	if len(skills) > 0 {
		tools.RegisterSkillTool(registry, skills)
	}

	mcpShutdown := func() {}
	explicitMCPConfig := f.mcpConfig != ""
	mcpConfigPath := resolveMCPConfig(f.mcpConfig, cwd)
	if _, statErr := os.Stat(mcpConfigPath); statErr == nil {
		mcpCfg, lerr := tools.LoadMCPConfig(mcpConfigPath)
		if lerr != nil {
			slog.Error("agent: load MCP config", "path", mcpConfigPath, "error", lerr)
			return 1
		}
		mcpShutdown, err = tools.ConnectMCP(ctx, registry, mcpCfg, outputPolicy)
		if err != nil {
			slog.Error("agent: connect MCP servers", "error", err)
			return 1
		}
	} else if explicitMCPConfig {
		// An explicitly-named --mcp-config that doesn't exist is a startup
		// error; the default <cwd>/.mcp.json is silently skipped when absent.
		slog.Error("agent: --mcp-config not found", "path", mcpConfigPath, "error", statErr)
		return 1
	}

	cfg := agent.Config{
		Model:                f.model,
		ThinkingBudget:       thinkingBudget,
		SystemPromptOverride: f.systemPrompt,
		AppendSystemPrompt:   f.appendSystemPrompt,
		ContextFiles:         contextFiles,
		SkillsInventory:      skillspkg.SkillsInventory(skills),
		Cwd:                  cwd,
		Ref:                  f.ref,
		Name:                 f.name,
		DBURL:                f.db,
		FakeTurns:            f.fakeTurns,
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		OpenRouterAPIKey:     os.Getenv("OPENROUTER_API_KEY"),
		Tools:                registry,
	}

	fe := agent.NewFrontend(os.Stdin, os.Stdout, nil)
	eng, engShutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		slog.Error("agent: build engine", "error", err)
		mcpShutdown()
		return 1
	}

	runErr := fe.Run()
	// Frontend.Run only returns once stdin hits EOF (or a scan error) or the
	// process is signalled - either way no further HandlePrompt/HandleSteer/
	// HandleAbort call can arrive, so Wait() then Close() is race-free (see
	// Engine.Close's doc comment).
	eng.Wait()
	eng.Close()
	mcpShutdown()
	engShutdown()

	if runErr != nil {
		slog.Error("agent: frontend run", "error", runErr)
		return 1
	}
	return 0
}
