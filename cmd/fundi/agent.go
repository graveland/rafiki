package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"git.graveland.dev/brent/fundi/internal/agent"
	"git.graveland.dev/brent/fundi/internal/agent/tools"
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
	provider           string
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

// parseAgentFlags parses the fundi agent flag set. It is a pure function of
// args plus the environment defaults ($PI_CONTROLLER_CHILD_ID for --ref,
// $FUNDI_AGENT_DB for --db) so it can be exercised directly by tests without
// a running agent.
func parseAgentFlags(args []string) (agentFlags, error) {
	var f agentFlags
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // runAgent reports parse errors itself

	fs.StringVar(&f.model, "model", "sonnet-latest", "model id or family alias")
	fs.StringVar(&f.provider, "provider", "", "primary upstream: anthropic|openrouter (default: openrouter if --model contains \"/\", else anthropic)")
	fs.StringVar(&f.thinking, "thinking", "off", "extended-thinking level: off|low|medium|high|xhigh")
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "override the base system prompt")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt")
	fs.BoolVar(&f.noContextFiles, "no-context-files", false, "skip loading CLAUDE.md/AGENTS.md context files")
	fs.Var(&f.skillsDir, "skills-dir", "additional skills directory (repeatable)")
	fs.StringVar(&f.skills, "skills", "", "comma-separated list restricting discovered skills to these names")
	fs.BoolVar(&f.noSkills, "no-skills", false, "disable skill discovery and the skill tool entirely")
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "path to .mcp.json (default: <cwd>/.mcp.json if present)")
	fs.StringVar(&f.ref, "ref", os.Getenv("PI_CONTROLLER_CHILD_ID"), "external ref correlating the conversation across restarts")
	fs.StringVar(&f.db, "db", os.Getenv("FUNDI_AGENT_DB"), "postgres url for conversation persistence (empty: in-memory)")
	fs.StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: os.TempDir()/fundi-spill-<ref>)")
	fs.StringVar(&f.name, "name", "", "session name reported through get_state")
	fs.StringVar(&f.fakeTurns, "fake-turns", "", "hidden test seam: path to a LoadFakeSender scripted-turns file")

	if err := fs.Parse(args); err != nil {
		return agentFlags{}, err
	}

	if f.provider == "" {
		f.provider = agent.DefaultProvider(f.model)
	}
	return f, nil
}

// runAgent is cmd/fundi's other entry point: `fundi agent ...` runs a single
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
		spillDir = filepath.Join(os.TempDir(), "fundi-spill-"+childID)
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

	var skills []agent.SkillMeta
	if !f.noSkills {
		home, herr := os.UserHomeDir()
		if herr != nil {
			slog.Warn("agent: could not resolve home directory; skipping user-global skills dir", "error", herr)
		}
		var dirs []string
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
		}
		dirs = append(dirs, filepath.Join(cwd, ".claude", "skills"))
		dirs = append(dirs, f.skillsDir...)

		var only []string
		if f.skills != "" {
			only = strings.Split(f.skills, ",")
		}
		skills, err = agent.DiscoverSkills(dirs, only)
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
	mcpConfigPath := f.mcpConfig
	explicitMCPConfig := mcpConfigPath != ""
	if mcpConfigPath == "" {
		mcpConfigPath = filepath.Join(cwd, ".mcp.json")
	}
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
		Provider:             f.provider,
		ThinkingBudget:       thinkingBudget,
		SystemPromptOverride: f.systemPrompt,
		AppendSystemPrompt:   f.appendSystemPrompt,
		ContextFiles:         contextFiles,
		SkillsInventory:      agent.SkillsInventory(skills),
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
