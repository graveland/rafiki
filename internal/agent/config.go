package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/agentloop"
	"git.graveland.dev/brent/rafiki/llm"
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

// DefaultProvider is the --provider default heuristic: an OpenRouter-native
// model id (one containing a slash, e.g. "meta-llama/llama-3.1-70b")
// implies OpenRouter; anything else defaults to Anthropic.
func DefaultProvider(model string) string {
	if strings.Contains(model, "/") {
		return "openrouter"
	}
	return "anthropic"
}

// Config is the fully-resolved input to BuildEngine. Every field is either a
// flag value taken as-is, or something cmd/fundi has already produced via
// I/O (env lookups, file reads, the assembled tool registry) before calling
// BuildEngine - BuildEngine itself parses no flags and does no filesystem
// discovery beyond opening the optional DB pool.
//
// Tools is a pre-built agentloop.ToolSet rather than something BuildEngine
// assembles itself: internal/agent/tools imports this package (for
// SkillMeta, in the skill tool), so this package can never import
// internal/agent/tools without an import cycle. cmd/fundi is where both
// sides meet - see cmd/fundi/agent.go, which builds the Registry (file
// tools, bash, skills, MCP) and hands it in here.
type Config struct {
	// Model is the model id or family alias (e.g. "sonnet-latest").
	Model string
	// Provider selects the primary upstream: "anthropic" or "openrouter".
	Provider string
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

	// DBURL, when non-empty, is a postgres connection string: BuildEngine
	// opens a pgxpool.Pool and wires llm.WithStore. Empty means an
	// in-memory (capture-less) conversation.
	DBURL string

	// FakeTurns, when non-empty, is a path to a LoadFakeSender script that
	// replaces the real upstream sender(s). This is the hidden --fake-turns
	// test seam: it drives BuildEngine end to end with no API key and no
	// network.
	FakeTurns string

	// AnthropicAPIKey / OpenRouterAPIKey are read from the environment by
	// cmd/fundi and passed in explicitly - rather than BuildEngine reading
	// os.Getenv itself - so tests can exercise the missing-key error path
	// without mutating the process environment.
	AnthropicAPIKey  string
	OpenRouterAPIKey string

	// Tools is the assembled tool registry (file tools + bash + skills +
	// MCP), built by cmd/fundi before calling BuildEngine.
	Tools agentloop.ToolSet
}

// BuildEngine constructs the llm.Client (and, when DBURL is set, its backing
// pgxpool.Pool), assembles the conversation options, and wires the Engine to
// fe. ctx becomes the Engine's BaseCtx - the root every turn's cancellable
// context derives from, so a caller deriving ctx from signal.NotifyContext
// gets SIGINT/SIGTERM cancellation of in-flight turns for free.
//
// The returned shutdown func closes the DB pool (a no-op when DBURL was
// empty). It does NOT close MCP sessions or the Engine's worker goroutine -
// those are cmd/fundi's to own (ConnectMCP's shutdown, Engine.Close) since
// this package cannot import internal/agent/tools. cmd/fundi/agent.go
// combines all three on its shutdown path.
func (c Config) BuildEngine(ctx context.Context, fe *Frontend) (*Engine, func(), error) {
	if c.Tools == nil {
		return nil, nil, errors.New("agent: Config.Tools is required")
	}

	primary := llm.Upstream(c.Provider)
	if primary != llm.UpstreamAnthropic && primary != llm.UpstreamOpenRouter {
		return nil, nil, fmt.Errorf("agent: unknown --provider %q (want anthropic|openrouter)", c.Provider)
	}

	clientOpts, err := c.senderOptions(primary)
	if err != nil {
		return nil, nil, err
	}

	var pool *pgxpool.Pool
	if c.DBURL != "" {
		pool, err = pgxpool.New(ctx, c.DBURL)
		if err != nil {
			return nil, nil, fmt.Errorf("agent: connect --db: %w", err)
		}
		clientOpts = append(clientOpts, llm.WithStore(pool))
	}
	shutdown := func() {
		if pool != nil {
			pool.Close()
		}
	}

	client, err := llm.NewClient(clientOpts...)
	if err != nil {
		shutdown()
		return nil, nil, fmt.Errorf("agent: build llm client: %w", err)
	}

	convOpts := []llm.ConvOption{
		llm.Entrypoint("agent"),
		llm.Model(c.Model),
		llm.Primary(primary),
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
	// Only meaningful when primary is Anthropic: SendParams forces OpenRouter
	// primary (with no fallback) whenever the model id itself contains a
	// slash, and senderOptions never enables the breaker fallback needs
	// unless the OpenRouter key is actually present.
	if primary == llm.UpstreamAnthropic && c.OpenRouterAPIKey != "" && c.FakeTurns == "" {
		convOpts = append(convOpts, llm.Fallback(llm.UpstreamOpenRouter))
	}

	eng, err := NewEngine(EngineConfig{
		Client:   client,
		ConvOpts: convOpts,
		Tools:    c.Tools,
		Provider: c.Provider,
		ModelID:  c.Model,
		Name:     c.Name,
		BaseCtx:  ctx,
	}, fe)
	if err != nil {
		shutdown()
		return nil, nil, fmt.Errorf("agent: build engine: %w", err)
	}
	return eng, shutdown, nil
}

// senderOptions builds the llm.ClientOptions wiring senders for the
// configured upstream(s), erroring out when the CHOSEN primary's key is
// missing (checked before any client/pool is constructed, so BuildEngine
// fails fast at startup rather than on the first turn).
func (c Config) senderOptions(primary llm.Upstream) ([]llm.ClientOption, error) {
	if c.FakeTurns != "" {
		fake, err := LoadFakeSender(c.FakeTurns)
		if err != nil {
			return nil, err
		}
		opts := []llm.ClientOption{llm.WithUpstream(llm.UpstreamAnthropic, fake)}
		if primary == llm.UpstreamOpenRouter {
			opts = append(opts, llm.WithUpstream(llm.UpstreamOpenRouter, fake))
		}
		return opts, nil
	}

	primaryKeyMissing := (primary == llm.UpstreamAnthropic && c.AnthropicAPIKey == "") ||
		(primary == llm.UpstreamOpenRouter && c.OpenRouterAPIKey == "")
	if primaryKeyMissing {
		return nil, fmt.Errorf("agent: no API key configured for --provider %s (set %s)", primary, envVarFor(primary))
	}

	var opts []llm.ClientOption
	if c.AnthropicAPIKey != "" {
		opts = append(opts, llm.WithUpstream(llm.UpstreamAnthropic, llm.Anthropic(c.AnthropicAPIKey)))
	} else {
		// llm.NewClient unconditionally requires an UpstreamAnthropic sender,
		// even when the configured provider is OpenRouter. Routing never
		// selects Anthropic in that case - no ANTHROPIC_API_KEY here means no
		// Fallback(UpstreamAnthropic) is ever configured either (see
		// BuildEngine) - so a stub that errors if it's ever actually invoked
		// is safe, and far clearer than a nil-sender panic if that invariant
		// is ever broken.
		opts = append(opts, llm.WithUpstream(llm.UpstreamAnthropic, unconfiguredSender{env: "ANTHROPIC_API_KEY"}))
	}
	if c.OpenRouterAPIKey != "" {
		// WithBreaker is what makes Fallback(UpstreamOpenRouter) actually
		// live: llm.Client.callModel bypasses the whole fallback chain
		// whenever the primary's breaker is nil, regardless of how many
		// fallbacks are configured. Mirrors rafiki's own cmd/rafiki/main.go,
		// which enables the breaker under the identical condition.
		opts = append(opts,
			llm.WithUpstream(llm.UpstreamOpenRouter, llm.OpenRouter(c.OpenRouterAPIKey)),
			llm.WithBreaker(15*time.Minute))
	}
	return opts, nil
}

func envVarFor(u llm.Upstream) string {
	if u == llm.UpstreamOpenRouter {
		return "OPENROUTER_API_KEY"
	}
	return "ANTHROPIC_API_KEY"
}

// unconfiguredSender is a placeholder llm.Sender satisfying llm.NewClient's
// mandatory UpstreamAnthropic registration when ANTHROPIC_API_KEY is unset
// and the configured provider is OpenRouter. See senderOptions for why it
// must never actually be called in that configuration; if it somehow is, it
// fails loudly rather than silently reaching a real call with no key.
type unconfiguredSender struct{ env string }

func (s unconfiguredSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, fmt.Errorf("agent: %s is not set; this sender must never be called", s.env)
}
