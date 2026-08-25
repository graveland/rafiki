package fundi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/store"
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
// flag value taken as-is, or something cmd/rafikid has already produced via
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
// cmd/rafikid builds the Registry and passes it in; see cmd/rafikid/agent.go.
type Config struct {
	// Model is the provider-qualified model id, e.g. "anthropic/sonnet-latest"
	// or "openrouter/deepseek/deepseek-chat". The first segment names the
	// configured provider; the rest is the provider-local model id.
	// cmd/rafikid requires --model to be provider-qualified (see
	// parseAgentFlags) - BuildEngine validates it via Config.Validate.
	Model string
	// ThinkingBudget is the resolved extended-thinking token budget (0
	// disables it) — see ThinkingBudgetFor.
	ThinkingBudget int64

	// MaxOutputTokens is the per-turn output cap sent as max_tokens to the
	// upstream API. Zero means use the default (16384). This is NOT a hard
	// limit the agent enforces — the upstream enforces it — and hitting it
	// is recoverable: the agent loop fails any truncated tool calls and
	// continues to give the model another turn budget.
	MaxOutputTokens int

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

	// Workspace, when non-nil and Isolation != "none", appends a per-child
	// machine block to the system prompt. Resolved by the daemon at spawn.
	Workspace *WorkspaceInfo

	// Ref correlates the conversation across restarts (llm.ByExternalRef);
	// empty means no correlation (a fresh conversation every run).
	Ref string
	// Name is the session name reported through get_state.
	Name string

	// Pool is the database pool backing conversation persistence, supplied by
	// the owning process. A nil Pool means an in-memory (capture-less)
	// conversation, exactly as an empty DBURL used to behave. BuildEngine
	// never opens or closes a pool itself — one daemon can share a single
	// Pool across N engines, and only the owner (cmd/rafikid's standalone CLI
	// path, or the daemon in Task 5) may open or close it.
	Pool *pgxpool.Pool

	// FakeTurns, when non-empty, is a path to a LoadFakeSender script that
	// replaces the real upstream sender(s). This is the hidden --fake-turns
	// test seam: it drives BuildEngine end to end with no API key and no
	// network.
	FakeTurns string

	// Providers is the resolved provider registry. cmd/rafikid loads it once
	// (paths.ProvidersFile) and shares one *providers.Set across every child.
	// Required: without it a model id cannot be resolved to a sender.
	Providers *providers.Set

	// ProviderSenders overrides the sender for specific providers by name —
	// see RuntimeOptions.ProviderSenders, whose doc comment carries the full
	// reasoning. nil means every provider dials as its providers.toml entry
	// says.
	ProviderSenders map[string]llm.Sender

	// APIKeyOverride, when non-empty, replaces the resolved credential for the
	// provider this child's model names — and for no other. It carries a
	// per-spawn key (SpawnRequest.APIKey) that the daemon's own environment
	// does not have. The provider is identified by Model, so a forwarded key
	// can never land on a provider the caller did not address.
	APIKeyOverride string

	// Tools is the assembled tool registry (file tools + bash + skills +
	// MCP), built by cmd/rafikid before calling BuildEngine.
	Tools agentloop.ToolSet

	// OnFatal is handed straight to EngineConfig.OnFatal: the owner's hook for
	// ending this child when a turn panics. See EngineConfig.OnFatal. Nil is
	// legal (a standalone `rafikid fundi` process has nothing to hand back to).
	OnFatal func(error)

	// AutoResume asks the engine to call agentloop.Resume before accepting
	// any inbound prompts — see EngineConfig.AutoResume.
	AutoResume bool

	// RawTrace, when non-nil, enables raw LLM API request/response capture.
	// Nil disables capture; passed directly to llm.WithRecordRequests.
	RawTrace *rawtrace.RawTraceStore

	// OnConversationResolved is called once, after the engine resolves its
	// conversation and before any turn can run, and returns the write lease for
	// that conversation.
	//
	// This is the ONLY lease-acquisition site. The conversation id does not
	// exist until NewEngine resolves it, so the daemon cannot acquire one
	// beforehand; and every path that runs an agent — spawn, resume, startup
	// recovery — reaches BuildEngine, so one hook here covers all three.
	//
	// An error refuses the engine: another daemon drives this conversation, and
	// starting anyway is the double-write the lease exists to prevent. Nil
	// means unfenced.
	OnConversationResolved func(ctx context.Context, conversationID string) (store.Lease, error)
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
// cmd/rafikid's to own (ConnectMCP's shutdown, Engine.Close). cmd/rafikid/agent.go
// combines all three (plus its own pool.Close()) on its shutdown path.
func (c Config) BuildEngine(ctx context.Context, fe *Frontend) (*Engine, func(), error) {
	if c.Tools == nil {
		return nil, nil, errors.New("agent: Config.Tools is required")
	}

	clientOpts, err := c.clientOptions()
	if err != nil {
		return nil, nil, err
	}

	pool := c.Pool
	if pool != nil {
		clientOpts = append(clientOpts, llm.WithStore(pool))
	}
	clientOpts = append(clientOpts, llm.WithRecordRequests(c.RawTrace))
	shutdown := func() {}

	client, err := llm.NewClient(clientOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: build llm client: %w", err)
	}

	p, modelID, err := c.Providers.Split(c.Model)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: %w", err)
	}

	convOpts := []llm.ConvOption{
		llm.Entrypoint("agent"),
		llm.Model(c.Model),
		llm.ThinkingBudget(c.ThinkingBudget),
		llm.WithName(c.Name),
		llm.SystemText(BuildSystemPrompt(SysPromptConfig{
			Base:            defaultBasePrompt,
			Override:        c.SystemPromptOverride,
			Append:          c.AppendSystemPrompt,
			ContextFiles:    c.ContextFiles,
			SkillsInventory: c.SkillsInventory,
			Cwd:             c.Cwd,
			ModelID:         c.Model,
			Workspace:       c.Workspace,
		})),
	}
	if c.Ref != "" {
		convOpts = append(convOpts, llm.ByExternalRef(c.Ref))
	}
	if c.MaxOutputTokens > 0 {
		convOpts = append(convOpts, llm.MaxTokens(int64(c.MaxOutputTokens)))
	}

	eng, err := NewEngine(EngineConfig{
		Client:     client,
		ConvOpts:   convOpts,
		Tools:      c.Tools,
		Provider:   p.Name,
		ModelID:    modelID,
		Name:       c.Name,
		BaseCtx:    ctx,
		AutoResume: c.AutoResume,
		OnFatal:    c.OnFatal,
	}, fe)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: build engine: %w", err)
	}

	// Acquire the write lease now: the conversation is resolved (eng.conv.ID is
	// set) and nothing has written yet. Ordering is load-bearing in both
	// directions — it must follow NewEngine, which resolves the id, and precede
	// RepairOrphans below, which WRITES.
	if c.OnConversationResolved != nil && pool != nil {
		lease, lerr := c.OnConversationResolved(ctx, eng.conv.ID)
		if lerr != nil {
			return nil, nil, fmt.Errorf("agent: conversation lease: %w", lerr)
		}
		client.SetLease(lease)
	}

	// Boot-time orphan repair: distinct from runTurn's abort-path repair
	// (engine.go), which only ever cleans up a turn cancelled WITHIN this
	// process. A DB-backed conversation reattached via ByExternalRef (see
	// convOpts above) can carry a dangling tool_use left by a PREVIOUS
	// process that crashed or was killed mid-turn — before that process's
	// own abort handling ever ran. Repair here, once, before the engine's
	// worker can execute any turn (NewEngine has already started it, but
	// nothing wakes it until cmd/rafikid's Frontend.Run reads its first
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
		} else if repaired > 0 {
			slog.Info("agent: boot-time orphan repair", "conversation", eng.conv.ID, "repaired", repaired)
		}
	}

	return eng, shutdown, nil
}

// Validate checks everything that can fail before any client, pool or network
// exists, so BuildEngine fails at startup rather than on the first turn.
func (c Config) Validate() error {
	if c.Providers == nil {
		return errors.New("agent: no provider registry configured (providers.toml)")
	}
	if _, _, err := c.Providers.Split(c.Model); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// clientOptions builds the llm.ClientOptions for this config. The old version
// of this required an ANTHROPIC_API_KEY unconditionally, because llm.NewClient
// did; a keyless local-only child is constructible now.
func (c Config) clientOptions() ([]llm.ClientOption, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	opts := []llm.ClientOption{llm.WithProviders(c.Providers)}

	if c.FakeTurns != "" {
		fake, err := LoadFakeSender(c.FakeTurns)
		if err != nil {
			return nil, err
		}
		// Every provider gets the fake: --fake-turns must survive a failover
		// as well as a direct send.
		for _, name := range c.Providers.Names() {
			opts = append(opts, llm.WithProviderSender(name, fake))
		}
		return opts, nil
	}

	if c.APIKeyOverride != "" {
		p, _, err := c.Providers.Split(c.Model)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		sender, err := llm.SenderForKey(p, c.APIKeyOverride, nil)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		opts = append(opts, llm.WithProviderSender(p.Name, sender))
	}

	// c.ProviderSenders wins last, and unconditionally, for the names it
	// carries: cmd/rafikid only puts a provider in this map after resolving
	// its via_executor relay, and that resolution — not the direct dial
	// APIKeyOverride's SenderForKey(p, key, nil) would otherwise build — is
	// the one allowed to reach that provider's base_url. A provider absent
	// from this map (the common case: no via_executor configured) is
	// unaffected.
	for name, sender := range c.ProviderSenders {
		opts = append(opts, llm.WithProviderSender(name, sender))
	}
	return opts, nil
}

// shutdown := func() {} (no-op) kept for signature stability.
