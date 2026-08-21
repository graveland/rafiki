package main

import (
	"context"
	"errors"
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
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
)

// stringSliceFlag implements flag.Value for a repeatable flag (--skills-dir).
// DELETED in favour of pflag's built-in StringArrayVar. See usage.go.

// agentFlags is the parsed --model .. --fake-turns flag contract Task 16's
// buildAgentArgv targets verbatim. See parseAgentFlags.
type agentFlags struct {
	model              string
	thinking           string
	maxOutputTokens    int
	systemPrompt       string
	appendSystemPrompt string
	noContextFiles     bool
	skillsDir          []string
	skills             string
	noSkills           bool
	mcpConfig          string
	lspConfig          string
	noLSP              bool
	ref                string
	db                 string
	spillDir           string
	name               string
	fakeTurns          string
	recordRequests     bool
	bashRTK            string
	toolsWeb           bool
	// toolsWebSet records whether --tools-web appeared in argv at all, which
	// a bool's value alone cannot express. See toolsWebValue.
	toolsWebSet bool
}

// parseAgentFlags parses the rafikid agent flag set. It is a pure function of
// args plus the environment defaults ($RAFIKI_CHILD_ID for --ref,
// $RAFIKI_DB for --db) so it can be exercised directly by tests without
// a running agent.
func parseAgentFlags(args []string) (agentFlags, error) {
	var f agentFlags
	fs := newAgentFlagSet(&f) // shared with printAgentUsage — see usage.go

	if err := fs.Parse(args); err != nil {
		return agentFlags{}, err
	}

	// pflag: use Changed to detect whether --tools-web appeared in argv.
	if fl := fs.Lookup("tools-web"); fl != nil {
		f.toolsWebSet = fl.Changed
	}

	if f.model == "" || !strings.Contains(f.model, "/") {
		return agentFlags{}, fmt.Errorf(`--model is required and must be provider-qualified, e.g. "anthropic/sonnet-latest" or "deepseek/deepseek-chat"`)
	}
	return f, nil
}

// assembleSkillDirs builds the skill search path.
//
// hasExecutor drops the two project-tier directories: cwd names a path on the
// EXECUTOR, so resolving it here finds either nothing or, on a daemon that
// happens to have a directory at the same path, a different project's skills
// presented as this one's. The project tier comes over the executor link
// instead (fetchProjectSkills).
func assembleSkillDirs(cwd string, flagDirs []string, hasExecutor bool) []string {
	dirs := paths.SkillsDirs()
	if !hasExecutor {
		dirs = append(dirs, filepath.Join(cwd, ".claude", "skills"))
		dirs = append(dirs, filepath.Join(cwd, ".rafiki", "skills"))
	}
	return append(dirs, flagDirs...)
}

// bashRTKValue resolves the --bash-rtk flag precedence: explicit flag beats
// $RAFIKI_BASH_RTK beats the "auto" default.
func bashRTKValue(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := paths.Get(paths.BashRTK); env != "" {
		return env
	}
	return "auto"
}

// effectiveLSPConfig resolves the lsp.json path a runtime should actually
// load, encoding the one precedence rule that was previously copy-pasted
// between runAgent (agent.go) and toRuntimeOptions (agent_runtime.go): an
// explicit --lsp-config must reach BuildRuntime even when the path does not
// exist, so a typo'd flag value is a hard startup error there rather than a
// silent no-op; a path that only came from resolveLSPConfig's own default (no
// --lsp-config given) must be blanked to "" when absent, so a cwd/machine with
// no lsp.json simply runs without LSP tools instead of erroring on a file
// nobody asked for. Both call sites now call this instead of hand-rolling the
// os.Stat check, so they cannot drift the way finding 15 flagged.
func effectiveLSPConfig(flagValue, cwd string) string {
	lspPath := resolveLSPConfig(flagValue, cwd)
	if flagValue == "" {
		if _, err := os.Stat(lspPath); err != nil {
			lspPath = "" // defaulted path absent: skip LSP, as before
		}
	}
	return lspPath
}

// toolsWebValue resolves the --tools-web precedence: an explicitly passed flag
// beats $RAFIKI_TOOLS_WEB, which beats the default of OFF.
//
// It takes the value AND whether the flag was actually passed, because a bool's
// value alone cannot carry that: --tools-web=false and an absent flag both
// leave false, and collapsing them would make it impossible to turn the tools
// off from the command line when $RAFIKI_TOOLS_WEB=1 — precisely the case this
// precedence rule exists for. parseAgentFlags supplies the second argument from
// flag.FlagSet.Visit, which walks only the flags that were set.
//
// Deliberately NOT a string flag in the shape of --bash-rtk. That knob is
// genuinely tri-valued (auto/on/off) so a string fits it; this one is boolean,
// and stdlib's flag package resolves `--tools-web --model X` for a string flag
// by taking "--model" as the value and leaving --model unset, with no error
// reported. A bool flag rejects that spelling instead of silently eating the
// next argument.
func toolsWebValue(flagVal, flagPassed bool) bool {
	if flagPassed {
		return flagVal
	}
	return paths.Get(paths.ToolsWeb) == "1"
}

// standaloneFatal builds the EngineConfig.OnFatal hook for `rafikid agent`, plus
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

// runAgentWithFlags is the post-flag-parsing body, shared by both the
// cobra path (newFundiCmd.RunE) and the in-process daemon path
// (agentRuntimeOptions → parseAgentFlags → toRuntimeOptions).
//
// Exit codes: 0 or the reader's clean EOF/graceful-shutdown path, 1 for a
// setup or run error, 2 for a thinking-budget validation error.
func runAgentWithFlags(f agentFlags) int {

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
	// flight, per the engine lifecycle fix in pkg/fundi/engine.go.
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

	lspPath := effectiveLSPConfig(f.lspConfig, cwd)
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
		MaxOutputTokens:      f.maxOutputTokens,
		SystemPromptOverride: f.systemPrompt,
		AppendSystemPrompt:   f.appendSystemPrompt,
		Cwd:                  cwd,
		Ref:                  f.ref,
		Name:                 f.name,
		SpillDir:             f.spillDir,
		SkillsDirs:           assembleSkillDirs(cwd, f.skillsDir, false),
		Skills:               f.skills,
		NoSkills:             f.noSkills,
		NoContextFiles:       f.noContextFiles,
		MCPConfig:            mcpPath,
		LSPConfig:            lspPath,
		FakeTurns:            f.fakeTurns,
		Providers:            providers.Default(),
		Pool:                 pool,
		OnFatal:              onFatal,
		RTK:                  bashRTKValue(f.bashRTK),
		ToolsWeb:             toolsWebValue(f.toolsWeb, f.toolsWebSet),
		NoLSP:                f.noLSP || paths.Get(paths.LSPDisable) == "1",
		// The standalone CLI is its own workspace: no daemon, no pool, nobody
		// else's children. It satisfies the executor rule with a real
		// in-process client rather than an exemption, so there is one rule.
		InProcessWorkspace: true,
	}
	if f.recordRequests {
		// NewRawTraceStore(nil) is documented to return nil, so this is safe
		// even when --db was not given (opts.RawTrace then stays nil, same as
		// not passing --record-requests at all).
		opts.RawTrace = rawtrace.NewRawTraceStore(pool)
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
