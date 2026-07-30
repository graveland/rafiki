package main

import (
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// appendDaemonRef appends the authoritative --ref last so it wins over
// req.ExtraArgs under buildAgentArgv's last-flag-wins convention. A subprocess
// child receives its id through the injected FUNDI_CHILD_ID env var; an
// in-process child inherits no env, so the id must travel in argv. It must not
// be caller-overridable: --ref selects which stored conversation the child
// reattaches, so a spoofed value points one child at another's history.
func appendDaemonRef(argv []string, childID string) []string {
	return append(argv, "--ref", childID)
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
func (f agentFlags) toRuntimeOptions(cwd string, pool *pgxpool.Pool) (agent.RuntimeOptions, error) {
	thinkingBudget, err := agent.ThinkingBudgetFor(f.thinking)
	if err != nil {
		return agent.RuntimeOptions{}, err
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

	return agent.RuntimeOptions{
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
	}, nil
}
