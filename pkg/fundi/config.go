package fundi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/llm"
)

// thinkingBudgets maps the --thinking flag's named levels to the Anthropic
// extended-thinking token budget. "off" (and the zero value) disables
// thinking entirely.
var thinkingBudgets = map[string]int64{
	"":       0,
	"off":    0,
	"low":    4096,
	"medium": 8192,
	"high":   16384,
	"xhigh":  32768,
}

// ThinkingBudgetFor maps a --thinking level to its token budget. An unknown
// level is a returned error, not a silent fallback to "off" - a typo in the
// flag should fail fast at startup, not quietly disable thinking.
func ThinkingBudgetFor(level string) (int64, error) {
	budget, ok := thinkingBudgets[level]
	if !ok {
		return 0, fmt.Errorf("agent: unknown --thinking level %q (want off|low|medium|high|xhigh)", level)
	}
	return budget, nil
}

// Config is the fully-resolved input to BuildEngine. Every field is either a
// flag value taken as-is, or something cmd/fundid has already produced via
// I/O (env lookups, file reads, the assembled tool registry, the shared DB
// pool) before calling BuildEngine - BuildEngine itself parses no flags and
// does no filesystem or network discovery.
//
// Tools is a pre-built agentloop.ToolSet so callers can assemble a registry
// (file tools, bash, skills, MCP) and hand it in; this decouples BuildEngine
// from the concrete registry and lets tests inject a fake toolset. This
// architecture was originally forced by an import cycle (internal/fundi/tools
// importing this package for SkillMeta), which the extraction of internal/skills
// removed. The interface design remains the right choice for the decoupling alone.
// cmd/fundid builds the Registry and passes it in; see cmd/fundid/agent.go.
type Config struct {
	// Model is the provider-qualified model id, e.g. "anthropic/sonnet-latest"
	// or "deepseek/deepseek-chat". rafiki routes on this id alone: an
	// "anthropic/" prefix resolves through the native Anthropic sender
	// (prefix stripped, remainder resolved as an alias or concrete id);
	// anything else routes to OpenRouter. cmd/fundid requires --model to be
	// provider-qualified (see parseAgentFlags) - BuildEngine itself does not
	// re-validate that, so a bare id here (as some pre-redesign unit tests
	// still use) is passed straight to rafiki uninterpreted.
	Model string
	// ThinkingBudget is the resolved extended-thinking token budget (0
	// disables it) - see ThinkingBudgetFor.
	ThinkingBudget int64

	// System prompt sections assembled via BuildSystemPrompt.
	// SystemPromptOverride replaces the runtime's default base prompt when
	// non-empty (SpawnRequest.SystemPrompt / --system-prompt);
	// AppendSystemPrompt, ContextFiles and SkillsInventory are optional and
	// omitted when empty.
	SystemPromptOverride string
	AppendSystemPrompt   string
	ContextFiles         string
	SkillsInventory      string
	// Cwd is reported in the system prompt's environment block.
	Cwd string

	// Ref correlates the conversation across restarts (llm.ByExternalRef);
	// empty means no correlation (a fresh conversation every run).
	Ref string
	// Name is the session name reported through get_state.
	Name string

	// Pool is the database pool backing conversation persistence, supplied by
	// the owning process. A nil Pool means an in-memory (capture-less)
	// conversation, exactly as an empty DBURL used to behave. BuildEngine
	// never opens or closes a pool itself — one daemon can share a single
	// Pool across N engines, and only the owner (cmd/fundid's standalone CLI
	// path, or the daemon in Task 5) may open or close it.
	Pool *pgxpool.Pool

	// FakeTurns, when non-empty, is a path to a LoadFakeSender script that
	// replaces the real upstream sender(s). This is the hidden --fake-turns
	// test seam: it drives BuildEngine end to end with no API key and no
	// network.
	FakeTurns string

	// AnthropicAPIKey / OpenRouterAPIKey are read from the environment by
	// cmd/fundid and passed in explicitly - rather than BuildEngine reading
	// os.Getenv itself - so tests can exercise the missing-key error path
	// without mutating the process environment.
	AnthropicAPIKey  string
	OpenRouterAPIKey string

	// Tools is the assembled tool registry (file tools + bash + skills +
	// MCP), built by cmd/fundid before calling BuildEngine.
	Tools agentloop.ToolSet

	// OnFatal is handed straight to EngineConfig.OnFatal: the owner's hook for
	// ending this child when a turn panics. See EngineConfig.OnFatal. Nil is
	// legal (a standalone `fundid agent` process has nothing to hand back to).
	OnFatal func(error)
}

// BuildEngine constructs the llm.Client (wiring c.Pool via llm.WithStore when
// non-nil), assembles the conversation options, and wires the Engine to fe.
// ctx becomes the Engine's BaseCtx - the root every turn's cancellable
// context derives from, so a caller deriving ctx from signal.NotifyContext
// gets SIGINT/SIGTERM cancellation of in-flight turns for free.
//
// BuildEngine never opens or closes c.Pool - the owning process does, so N
// engines in one daemon can share one pool. The returned shutdown func is
// therefore a no-op today, kept for signature stability; it does NOT close
// MCP sessions or the Engine's worker goroutine either - those are
// cmd/fundid's to own (ConnectMCP's shutdown, Engine.Close). cmd/fundid/agent.go
// combines all three (plus its own pool.Close()) on its shutdown path.
func (c Config) BuildEngine(ctx context.Context, fe *Frontend) (*Engine, func(), error) {
	if c.Tools == nil {
		return nil, nil, errors.New("agent: Config.Tools is required")
	}

	clientOpts, err := c.senderOptions()
	if err != nil {
		return nil, nil, err
	}

	pool := c.Pool
	if pool != nil {
		clientOpts = append(clientOpts, llm.WithStore(pool))
	}
	shutdown := func() {}

	client, err := llm.NewClient(clientOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: build llm client: %w", err)
	}

	convOpts := []llm.ConvOption{
		llm.Entrypoint("agent"),
		llm.Model(c.Model),
		llm.ThinkingBudget(c.ThinkingBudget),
		llm.SystemText(BuildSystemPrompt(SysPromptConfig{
			Base:            defaultBasePrompt,
			Override:        c.SystemPromptOverride,
			Append:          c.AppendSystemPrompt,
			ContextFiles:    c.ContextFiles,
			SkillsInventory: c.SkillsInventory,
			Cwd:             c.Cwd,
			ModelID:         c.Model,
		})),
	}
	if c.Ref != "" {
		convOpts = append(convOpts, llm.ByExternalRef(c.Ref))
	}
	// rafiki routes on the model id alone (an "anthropic/" prefix always
	// wins native Anthropic; SendParams zeroes the fallback on its own for an
	// OpenRouter-primary/slash model), so fundi needs no per-model branching
	// here: whenever an OpenRouter key is configured, offer it as a fallback.
	if c.OpenRouterAPIKey != "" && c.FakeTurns == "" {
		convOpts = append(convOpts, llm.Fallback(llm.UpstreamOpenRouter))
	}

	provider, modelID := splitModel(c.Model)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		ConvOpts: convOpts,
		Tools:    c.Tools,
		Provider: provider,
		ModelID:  modelID,
		Name:     c.Name,
		BaseCtx:  ctx,
		OnFatal:  c.OnFatal,
	}, fe)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: build engine: %w", err)
	}

	// Boot-time orphan repair: distinct from runTurn's abort-path repair
	// (engine.go), which only ever cleans up a turn cancelled WITHIN this
	// process. A DB-backed conversation reattached via ByExternalRef (see
	// convOpts above) can carry a dangling tool_use left by a PREVIOUS
	// process that crashed or was killed mid-turn — before that process's
	// own abort handling ever ran. Repair here, once, before the engine's
	// worker can execute any turn (NewEngine has already started it, but
	// nothing wakes it until cmd/fundid's Frontend.Run reads its first
	// inbound frame, which happens strictly after BuildEngine returns).
	// In-memory mode (c.Pool == nil) has nothing to reattach — Conversation
	// always mints a fresh "mem-..." id — so this is a clean no-op there,
	// not an error.
	if pool != nil {
		repairCtx, repairCancel := context.WithTimeout(context.Background(), repairTimeout)
		repaired, rErr := RepairOrphans(repairCtx, eng.conv)
		repairCancel()
		if rErr != nil {
			slog.Error("agent: boot-time orphan repair failed", "conversation", eng.conv.ID, "error", rErr)
		} else {
			slog.Info("agent: boot-time orphan repair", "conversation", eng.conv.ID, "repaired", repaired)
		}
	}

	return eng, shutdown, nil
}

// senderOptions builds the llm.ClientOptions wiring senders for the
// configured upstream(s). rafiki's llm.NewClient unconditionally requires an
// UpstreamAnthropic sender, so ANTHROPIC_API_KEY is always mandatory
// regardless of which model is configured; OPENROUTER_API_KEY is only
// mandatory when the model needs OpenRouter (i.e. isn't "anthropic/"
// prefixed). Both checks run before any client/pool is constructed, so
// BuildEngine fails fast at startup rather than on the first turn.
func (c Config) senderOptions() ([]llm.ClientOption, error) {
	needsOpenRouter := !strings.HasPrefix(c.Model, "anthropic/")

	if c.FakeTurns != "" {
		fake, err := LoadFakeSender(c.FakeTurns)
		if err != nil {
			return nil, err
		}
		opts := []llm.ClientOption{llm.WithUpstream(llm.UpstreamAnthropic, fake)}
		if needsOpenRouter {
			opts = append(opts, llm.WithUpstream(llm.UpstreamOpenRouter, fake))
		}
		return opts, nil
	}

	if c.AnthropicAPIKey == "" {
		return nil, errors.New("agent: ANTHROPIC_API_KEY is required (rafiki's llm.NewClient always needs an Anthropic sender)")
	}
	if needsOpenRouter && c.OpenRouterAPIKey == "" {
		return nil, fmt.Errorf("agent: model %q requires OPENROUTER_API_KEY", c.Model)
	}

	opts := []llm.ClientOption{llm.WithUpstream(llm.UpstreamAnthropic, llm.Anthropic(c.AnthropicAPIKey))}
	if c.OpenRouterAPIKey != "" {
		// WithBreaker is what makes Fallback(UpstreamOpenRouter) actually
		// live: llm.Client.callModel bypasses the whole fallback chain
		// whenever the primary's breaker is nil, regardless of how many
		// fallbacks are configured. Mirrors the daemon's own cmd/fundid/proxy.go,
		// which enables the breaker under the identical condition.
		opts = append(opts,
			llm.WithUpstream(llm.UpstreamOpenRouter, llm.OpenRouter(c.OpenRouterAPIKey)),
			llm.WithBreaker(15*time.Minute))
	}
	return opts, nil
}

// splitModel splits a provider-qualified model id ("anthropic/sonnet-latest")
// into the provider label and model id reported through get_state
// (EngineConfig.Provider/ModelID) - a bare id with no slash reports an empty
// provider, which only happens when a caller constructs Config directly
// (e.g. pre-redesign unit tests) rather than through parseAgentFlags, which
// requires --model to be provider-qualified.
func splitModel(model string) (provider, id string) {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}
