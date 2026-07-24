package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"git.graveland.dev/brent/rafiki/routing"
	"git.graveland.dev/brent/rafiki/store"

	"github.com/timescale/savannah-common/go/tslogs"
)

// Client is the long-lived library handle: configured upstreams, per-upstream
// breakers, the capture/conversation store, the model catalog and tracing.
// Conversation-level defaults live on Conversation; per-send overrides on
// Send.
type Client struct {
	senders  map[Upstream]Sender
	breakers map[Upstream]*routing.Breaker
	pool     *pgxpool.Pool
	capture  *routing.CaptureStore
	messages *store.Messages
	catalog  *routing.ModelCatalog
	logger   *tslogs.Logger
	tracer   trace.Tracer

	breakerWindow time.Duration
	defaultModel  string
}

type ClientOption func(*Client)

// WithUpstream configures a Sender for an upstream. UpstreamAnthropic is the
// default primary; UpstreamOpenRouter enables failover when a conversation
// requests it.
func WithUpstream(u Upstream, s Sender) ClientOption {
	return func(c *Client) { c.senders[u] = s }
}

// WithStore wires the Postgres pool for capture + DB-backed conversations.
// Required for Conversation; SendParams degrades to capture-less without it.
func WithStore(pool *pgxpool.Pool) ClientOption {
	return func(c *Client) { c.pool = pool }
}

// WithBreaker enables per-upstream circuit breakers with the given probe
// interval (one breaker per configured upstream; a storm on one face pins the
// same breaker every consumer in this process consults — sc's intentional
// shared-health design).
func WithBreaker(probeInterval time.Duration) ClientOption {
	return func(c *Client) { c.breakerWindow = probeInterval }
}

// WithCatalog shares an existing OpenRouter model catalog (e.g. sc's server
// catalog). Without it the client constructs its own (TTL 1h).
func WithCatalog(cat *routing.ModelCatalog) ClientOption {
	return func(c *Client) { c.catalog = cat }
}

// WithLogger sets the library logger; defaults to error-level.
func WithLogger(l *tslogs.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

// WithDefaultModel sets the model used when a conversation doesn't specify
// one. There is no built-in default: without this option and with no
// per-conversation model, resolution errors rather than silently picking one.
func WithDefaultModel(m string) ClientOption {
	return func(c *Client) { c.defaultModel = m }
}

// WithTracerProvider injects OpenTelemetry tracing. The library never
// installs a global provider: embedded mode passes the host's provider,
// the standalone binary constructs one from OTLP env vars. Omitted = no-op.
func WithTracerProvider(tp trace.TracerProvider) ClientOption {
	return func(c *Client) { c.tracer = tp.Tracer("git.graveland.dev/brent/rafiki/llm") }
}

func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		senders:  map[Upstream]Sender{},
		breakers: map[Upstream]*routing.Breaker{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.senders[UpstreamAnthropic] == nil {
		return nil, errors.New("llm: an UpstreamAnthropic sender is required (WithUpstream)")
	}
	if c.logger == nil {
		logger, err := tslogs.NewLogger(tslogs.LevelError, false, "rafiki-llm", 0)
		if err != nil {
			return nil, fmt.Errorf("llm: default logger: %w", err)
		}
		c.logger = logger
	}
	if c.tracer == nil {
		c.tracer = noop.NewTracerProvider().Tracer("git.graveland.dev/brent/rafiki/llm")
	}
	if c.catalog == nil {
		c.catalog = routing.NewModelCatalog(http.DefaultClient, time.Hour, c.logger)
	}
	if c.breakerWindow > 0 {
		for u := range c.senders {
			c.breakers[u] = routing.NewBreaker(c.breakerWindow)
		}
	}
	if c.pool != nil {
		c.capture = routing.NewCaptureStore(c.pool)
		c.messages = store.NewMessages(c.pool)
	}
	return c, nil
}

// Breaker exposes the breaker for an upstream so the embedded proxy face can
// share it (one health signal per upstream per process). Nil when breakers
// are disabled or the upstream isn't configured.
func (c *Client) Breaker(u Upstream) *routing.Breaker { return c.breakers[u] }

// Catalog exposes the model catalog (shared with the proxy face).
func (c *Client) Catalog() *routing.ModelCatalog { return c.catalog }

// Pool exposes the store pool for host recovery sweeps
// (store.UnfinishedConversations). Nil without WithStore.
func (c *Client) Pool() *pgxpool.Pool { return c.pool }

// SendMeta carries per-send capture attribution and routing selection, used
// by hosts that build their own params (sc's core-dump analyzer) and
// internally by Conversation.Send.
type SendMeta struct {
	ConversationID   string
	OriginEntrypoint string
	DrivenBy         store.DrivenBy
	Owner            string
	Persona          string
	ExternalRef      string
	Ordinal          int
	Source           string
	Author           string
	AuthorKind       string

	Primary  Upstream   // empty = UpstreamAnthropic
	Fallback []Upstream // tried in order on retryable primary failure
}

// SendParams write-aheads the turn, routes the call through the primary's
// breaker with fallback, and completes/fails the turn. Capture is
// best-effort: a broken store degrades to pass-through, never blocks the
// call. Every invocation inserts its own turn row and always resolves it.
func (c *Client) SendParams(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	primary := meta.Primary
	if primary == "" {
		primary = UpstreamAnthropic
	}
	fallbacks := meta.Fallback
	// An "anthropic/<x>" id explicitly names the native Anthropic sender: strip
	// the prefix so it isn't caught by the generic slash->OpenRouter rule below,
	// and so the native API receives a bare id. This covers callers that build
	// params directly (bypassing ResolveModel); it's idempotent for the resolved
	// Conversation path, where the prefix is already gone.
	if stripped, ok := routing.StripNativeAnthropicPrefix(string(params.Model)); ok {
		params.Model = anthropic.Model(stripped)
	}
	if strings.Contains(string(params.Model), "/") {
		// A slash id is OpenRouter-native: route directly to OpenRouter rather
		// than defaulting to upstream anthropic. Keeps things compatible with
		// non anthropic models by using OpenRouter's logic.
		primary, fallbacks = UpstreamOpenRouter, nil
	}
	if primary == UpstreamOpenRouter {
		// Injected before capture so the recorded request matches the wire.
		applyProviderPrefs(&params)
	}
	ctx, span := c.tracer.Start(ctx, "llm.send", trace.WithAttributes(
		attribute.String("rafiki.model", string(params.Model)),
		attribute.String("rafiki.primary", string(primary)),
	))
	defer span.End()
	if b := c.breakers[primary]; b != nil {
		span.SetAttributes(attribute.Bool("rafiki.breaker.open", b.Open()))
	}

	turnID, turnCreatedAt, capturing := c.beginTurn(ctx, meta, params)

	start := time.Now()
	resp, servedBy, err := c.callModel(ctx, span, primary, fallbacks, params)
	latency := int(time.Since(start).Milliseconds())
	span.SetAttributes(attribute.String("rafiki.upstream", string(servedBy)))

	if err != nil {
		span.RecordError(err)
		if capturing {
			if ferr := c.capture.FailTurn(ctx, turnID, turnCreatedAt, err.Error()); ferr != nil {
				c.logger.Warn("capture: fail-turn write failed", "error", ferr)
			}
		}
		return nil, err
	}

	span.SetAttributes(
		attribute.Int64("rafiki.tokens.input", resp.Usage.InputTokens),
		attribute.Int64("rafiki.tokens.output", resp.Usage.OutputTokens),
		attribute.Int64("rafiki.tokens.cache_read", resp.Usage.CacheReadInputTokens),
		attribute.Int64("rafiki.tokens.cache_creation", resp.Usage.CacheCreationInputTokens),
	)

	if capturing {
		c.completeTurn(ctx, turnID, turnCreatedAt, resp, servedBy, latency)
	}
	return resp, nil
}

// callModel routes primary-with-breaker then the fallback chain. Fallback
// sends rewrite the model via the catalog (Anthropic id → OpenRouter id when
// the fallback is OpenRouter). A send with NO fallback configured bypasses
// the breaker entirely (direct primary, mirroring the routing core's
// fallback-less behavior) — per the design, an empty Fallback chain is how a
// consumer opts out of being pinned.
func (c *Client) callModel(ctx context.Context, span trace.Span, primary Upstream, fallbacks []Upstream, params anthropic.MessageNewParams) (*anthropic.Message, Upstream, error) {
	sender := c.senders[primary]
	if sender == nil {
		return nil, primary, fmt.Errorf("llm: upstream %q not configured", primary)
	}
	breaker := c.breakers[primary]
	now := time.Now()

	if len(fallbacks) == 0 || breaker == nil {
		resp, err := sender.New(ctx, params)
		return resp, primary, err
	}

	if breaker.UsePrimary(now) {
		resp, err := sender.New(ctx, params)
		if err == nil {
			breaker.RecordResult(now, false)
			return resp, primary, nil
		}
		if !routing.Retryable(err) {
			return nil, primary, err // non-retryable: don't fail over or trip
		}
		breaker.RecordResult(now, true)
		span.AddEvent("failover", trace.WithAttributes(attribute.String("rafiki.error", err.Error())))
		c.logger.Warn("primary failed; failing over", "primary", string(primary), "error", err)
	}

	var lastErr error
	for _, fb := range fallbacks {
		fbSender := c.senders[fb]
		if fbSender == nil {
			lastErr = fmt.Errorf("llm: fallback upstream %q not configured", fb)
			continue
		}
		fbParams := params
		if fb == UpstreamOpenRouter {
			fbParams.Model = anthropic.Model(c.catalog.OpenRouterModel(string(params.Model)))
			applyProviderPrefs(&fbParams)
		}
		resp, err := fbSender.New(ctx, fbParams)
		if err == nil {
			return resp, fb, nil
		}
		lastErr = err
	}
	return nil, primary, lastErr
}

// applyProviderPrefs injects OpenRouter provider-routing preferences for
// pinned model lines (routing.ProviderPrefsFor) as the request body's
// "provider" field. No-op for unpinned models; call only on params bound for
// OpenRouter — the field is not part of the Anthropic API.
func applyProviderPrefs(params *anthropic.MessageNewParams) {
	if prefs, ok := routing.ProviderPrefsFor(string(params.Model)); ok {
		params.SetExtraFields(map[string]any{"provider": prefs})
	}
}

// beginTurn resolves the conversation and write-aheads the request row.
// Mirrors the routing core's resolution rules: explicit ConversationID wins,
// else ExternalRef correlates, else a fresh conversation.
func (c *Client) beginTurn(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams) (string, time.Time, bool) {
	if c.capture == nil {
		return "", time.Time{}, false
	}
	drivenBy := meta.DrivenBy
	if drivenBy == "" {
		drivenBy = store.DrivenByServer
	}
	convRef := routing.ConversationRef{
		ID: meta.ConversationID, OriginEntrypoint: meta.OriginEntrypoint, DrivenBy: string(drivenBy),
		Owner: meta.Owner, Persona: meta.Persona, Model: string(params.Model), ExternalRef: meta.ExternalRef,
	}
	var convID string
	var err error
	switch {
	case meta.ConversationID != "":
		convID, err = c.capture.EnsureConversation(ctx, convRef)
	case meta.ExternalRef != "":
		convID, err = c.capture.EnsureConversationByExternalRef(ctx, convRef)
	default:
		convID, err = c.capture.EnsureConversation(ctx, convRef)
	}
	if err != nil {
		c.logger.Warn("capture: ensure-conversation failed (capturing disabled for this turn)", "error", err)
		return "", time.Time{}, false
	}
	reqJSON, mErr := json.Marshal(params)
	if mErr != nil {
		c.logger.Warn("capture: marshal request failed", "error", mErr)
		return "", time.Time{}, false
	}
	source := meta.Source
	if source == "" {
		source = meta.OriginEntrypoint
	}
	prefixHash := routing.PrefixHash(reqJSON)
	turnID, createdAt, iErr := c.capture.InsertTurnIntent(ctx, routing.TurnIntent{
		ConversationID: convID, Ordinal: meta.Ordinal, Model: string(params.Model), Request: reqJSON,
		Source: source, Author: meta.Author, AuthorKind: meta.AuthorKind,
		PrefixHash: prefixHash, Protocol: string(store.ProtocolAnthropic),
	})
	if iErr != nil {
		c.logger.Warn("capture: insert-intent failed (capturing disabled for this turn)", "error", iErr)
		return "", time.Time{}, false
	}
	// Record prefix_content (on-change) + cache_breakpoints for parity with the
	// proxy path — the message rows are written separately by Conversation, so
	// only the turn's prefix metadata is stored here (StoreTurnPrefix, not the
	// message-writing DecomposeRequest). Best-effort: never fails the turn.
	if err := c.capture.StoreTurnPrefix(ctx, convID, turnID, createdAt, reqJSON, prefixHash); err != nil {
		c.logger.Warn("capture: store turn prefix failed", "error", err)
	}
	return turnID, createdAt, true
}

// completeTurn records the response; on a completion/marshal failure the turn
// is failed rather than stranded pending (mirrors the routing core).
func (c *Client) completeTurn(ctx context.Context, turnID string, createdAt time.Time, resp *anthropic.Message, upstream Upstream, latencyMS int) {
	var cErr error
	reason := "complete-turn failed: "
	respJSON, mErr := json.Marshal(resp)
	if mErr != nil {
		cErr, reason = mErr, "capture serialization failed: "
		c.logger.Warn("capture: marshal response failed", "error", mErr)
	} else if cErr = c.capture.CompleteTurn(ctx, routing.TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Model: string(resp.Model), Response: respJSON,
		StopReason: string(resp.StopReason), Upstream: string(upstream),
		InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
		CacheReadTokens: resp.Usage.CacheReadInputTokens, CacheCreationTokens: resp.Usage.CacheCreationInputTokens,
		LatencyMS: latencyMS,
	}); cErr != nil {
		c.logger.Warn("capture: complete-turn write failed", "error", cErr)
	}
	if cErr != nil {
		if ferr := c.capture.FailTurn(ctx, turnID, createdAt, reason+cErr.Error()); ferr != nil {
			c.logger.Warn("capture: fail-turn fallback write failed", "error", ferr)
		}
	}
}
