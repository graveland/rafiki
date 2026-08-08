package fundi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/skills"
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
	MCPConfig            string // absolute path, or empty to skip MCP entirely
	FakeTurns            string
	AnthropicAPIKey      string
	OpenRouterAPIKey     string

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
	RawTrace *routing.RawTraceStore

	// RTK selects the bash tool's rtk mode: "auto", "on", or "off".
	// Empty means auto. See tools.ParseRTKMode.
	RTK string
}

// resolveContent loads the cwd-relative context files and discovers skills.
// Split out of BuildRuntime so a test can assert both were resolved from
// opts.Cwd and not from the process working directory — the regression this
// function's caller exists to prevent, and one that is otherwise invisible
// because LoadContextFiles skips absent files silently rather than erroring.
func resolveContent(opts RuntimeOptions) (contextFiles string, discovered []skills.SkillMeta, err error) {
	if !opts.NoContextFiles {
		contextFiles, err = LoadContextFiles(opts.Cwd)
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
	}

	return contextFiles, discovered, nil
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

	toolOpts := tools.ToolOpts{
		Cwd:          opts.Cwd,
		FileTracker:  tools.NewFileTracker(),
		OutputPolicy: outputPolicy,
		Skills:       discovered,
		RTK:          tools.ParseRTKMode(opts.RTK),
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
		Ref:                  opts.Ref,
		Name:                 opts.Name,
		FakeTurns:            opts.FakeTurns,
		AnthropicAPIKey:      opts.AnthropicAPIKey,
		OpenRouterAPIKey:     opts.OpenRouterAPIKey,
		Pool:                 opts.Pool,
		Tools:                registry,
		AutoResume:           opts.AutoResume,
		OnFatal:              opts.OnFatal,
		RawTrace:             opts.RawTrace,
	}

	eng, engShutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		mcpShutdown()
		return nil, nil, fmt.Errorf("runtime: build engine: %w", err)
	}
	return eng, func() {
		mcpShutdown()
		engShutdown()
	}, nil
}
