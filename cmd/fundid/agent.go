package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/paths"
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
// args plus the environment defaults ($RAFIKI_CHILD_ID for --ref,
// $RAFIKI_DB for --db) so it can be exercised directly by tests without
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
// the configured dirs (paths.SkillsDirs, which is rafiki's own config
// location - see that function's doc comment for why it is not
// ~/.claude/skills), then the project's existing <cwd>/.claude/skills (kept
// so a repo that already has one doesn't silently lose its skills), then the
// project's own <cwd>/.rafiki/skills (which wins on name collision with
// .claude/skills - skills.DiscoverSkills lets later entries override
// earlier ones), then any --skills-dir flags. Pure so it is testable.
func assembleSkillDirs(cwd string, flagDirs []string) []string {
	dirs := paths.SkillsDirs()
	dirs = append(dirs, filepath.Join(cwd, ".claude", "skills"))
	dirs = append(dirs, filepath.Join(cwd, ".rafiki", "skills"))
	return append(dirs, flagDirs...)
}

// standaloneFatal builds the EngineConfig.OnFatal hook for `fundid agent`, plus
// the channel runAgent reads to learn that it fired.
//
// A nil OnFatal is documented as legal and means "log it and stop taking
// turns" — but for this process that is a wedge, not a constraint. The Frontend
// keeps reading frames and answering get_state, so the caller still sees a
// healthy idle child while every prompt from that point on is accepted and
// silently never run: the exact silently-stopped-queue shape the daemon path
// was fixed to avoid. It was a choice here, not a limitation.
//
// Ending the process cleanly means unblocking Frontend.Run, which is parked in a
// read on stdin. Closing that is the only way to do it — the same mechanism
// inproc.Runner.engineFatal uses on the daemon path. The resulting Frontend.Run
// error is then OURS, not the engine's, which is why runAgent checks the
// returned channel first and reports the fatal error instead (mirroring
// inproc.Runner's stopReason).
//
// The hook is idempotent: OnFatal is once-only by contract, but the close and
// the buffered send must not be able to double-fire regardless of who calls it.
func standaloneFatal(stdin io.Closer) (func(error), <-chan error) {
	fired := make(chan error, 1)
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			slog.Error("agent: engine reported a fatal error; ending the process", "error", err)
			fired <- err
			if cerr := stdin.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
				slog.Warn("agent: close stdin after engine fatal", "error", cerr)
			}
		})
	}, fired
}

// runAgent is cmd/fundid's other entry point: `fundid agent ...` runs a single
// agent child speaking pi's rpc protocol on stdio, in place of Claude Code.
// It owns everything fundi.BuildRuntime cannot: flag parsing, os.Getwd(), the
// process-level signal.NotifyContext, and the CLI-only "--mcp-config not
// found" vs "defaulted .mcp.json absent" distinction (see the mcpPath block
// below). BuildRuntime owns the rest of the assembly (context files, skills,
// the tool registry, MCP connections, the Engine) so the daemon can build the
// same runtime per child without touching cwd or installing signal handlers.
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

	thinkingBudget, err := fundi.ThinkingBudgetFor(f.thinking)
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
	// flight, per the engine lifecycle fix in internal/fundi/engine.go.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mcpPath := resolveMCPConfig(f.mcpConfig, cwd)
	explicitMCP := f.mcpConfig != ""
	if _, err := os.Stat(mcpPath); err != nil {
		if explicitMCP {
			slog.Error("agent: --mcp-config not found", "path", mcpPath, "error", err)
			return 1
		}
		mcpPath = "" // defaulted path absent: skip MCP, as before
	}

	// The standalone CLI owns its pool: BuildRuntime/BuildEngine never open
	// or close one themselves (see Config.Pool's doc comment), so a daemon
	// can share a single pool across N engines. A nil pool here (--db unset)
	// means an in-memory conversation, matching the old empty-DBURL behaviour.
	var pool *pgxpool.Pool
	if f.db != "" {
		pool, err = pgxpool.New(ctx, f.db)
		if err != nil {
			slog.Error("agent: open database", "error", err)
			return 1
		}
		defer pool.Close()
	}

	onFatal, fatal := standaloneFatal(os.Stdin)

	opts := fundi.RuntimeOptions{
		Model:                f.model,
		ThinkingBudget:       thinkingBudget,
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
		OnFatal:              onFatal,
	}

	fe := fundi.NewFrontend(os.Stdin, os.Stdout, nil)
	eng, shutdown, err := fundi.BuildRuntime(ctx, fe, opts)
	if err != nil {
		slog.Error("agent: build engine", "error", err)
		return 1
	}

	runErr := fe.Run()
	// Frontend.Run only returns once stdin hits EOF (or a scan error) or the
	// process is signalled - either way no further HandlePrompt/HandleSteer/
	// HandleAbort call can arrive, so Wait() then Close() is race-free (see
	// Engine.Close's doc comment). Wait() cannot block on the turn that
	// panicked: Engine.fatal releases every outstanding count before calling
	// OnFatal.
	eng.Wait()
	eng.Close()
	shutdown()

	// An engine-fatal exit is reported as such, and takes precedence over the
	// runErr it caused (os.ErrClosed from the stdin close onFatal performed).
	select {
	case err := <-fatal:
		slog.Error("agent: exiting after a fatal engine error", "error", err)
		return 1
	default:
	}

	if runErr != nil {
		slog.Error("agent: frontend run", "error", runErr)
		return 1
	}
	return 0
}
