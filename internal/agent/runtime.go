package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/fundi/internal/agent/tools"
	"git.graveland.dev/brent/fundi/internal/paths"
	"git.graveland.dev/brent/fundi/internal/skills"
)

// RuntimeOptions is everything BuildRuntime needs to assemble an Engine. It is
// deliberately free of process globals: Cwd is explicit because the daemon's
// working directory is never the child's, and ctx is a parameter because a
// daemon must not install per-child signal handlers.
type RuntimeOptions struct {
	Model                string
	ThinkingBudget       int64
	SystemPromptOverride string
	AppendSystemPrompt   string
	Cwd                  string // must be absolute
	Ref                  string
	Name                 string
	SpillDir             string   // defaults to paths.SpillDir(Ref) when empty
	SkillsDirs           []string // already assembled; see assembleSkillDirs in cmd/fundid
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

	// OnFatal is the owner's hook for ending this child when a turn panics. It
	// is passed straight through to EngineConfig.OnFatal, whose doc comment
	// carries the contract. Nil is legal and means "log it and stop taking
	// turns" — correct for the standalone `fundid agent` process, wrong for
	// the daemon, which supplies one (inproc.Runner) so a panicked
	// conversation becomes an ordinary child exit rather than a wedged child.
	OnFatal func(error)
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

	registry := tools.NewRegistry()
	tools.RegisterFileTools(registry, tools.NewFileTracker())
	tools.RegisterBash(registry, outputPolicy, opts.Cwd)
	if len(discovered) > 0 {
		tools.RegisterSkillTool(registry, discovered)
	}

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
		OnFatal:              opts.OnFatal,
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
