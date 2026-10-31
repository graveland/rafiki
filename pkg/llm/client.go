// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.graveland.dev/rafiki/pkg/capture"
	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

// Client is the long-lived library handle: configured providers, per-provider
// breakers, the capture/conversation store, the model catalog and tracing.
// Conversation-level defaults live on Conversation; per-send overrides on
// Send.
type Client struct {
	senders  map[string]Sender
	breakers map[string]*routing.Breaker
	set      *providers.Set
	pool     *pgxpool.Pool
	capture  *capture.CaptureStore
	convs    *convpkg.Store
	// mem is the backend a store-less client's conversations live in. When no
	// conv store is supplied it IS convs' backend; when one is supplied it only
	// gives the "mem-" conversations a place to be created.
	mem     *convpkg.MemBackend
	lease   store.Lease
	catalog *routing.ModelCatalog
	logger  *slog.Logger
	tracer  trace.Tracer
	batcher Batcher

	breakerWindow time.Duration
	defaultModel  string
	routingSpec   routing.Spec
	// routingSource, when non-nil, is read once per request INSTEAD of the
	// frozen routingSpec: it is how a live steering write reaches a child's
	// next request rather than being stuck at its spawn-time value. See
	// WithRoutingSource and spec().
	routingSource func() routing.Spec
	modelGate     *ModelGate

	rawTrace rawTraceRecorder       // nil when disabled
	guard    *routing.ProviderGuard // nil = disabled (RAFIKI_PROVIDER_GUARD=off)

	// spanParent, when non-nil, supplies the parent for a span started from a
	// context that carries none. See WithSpanParent.
	spanParent func() trace.SpanContext
}

// rawTraceRecorder is the one method Client needs from *rawtrace.RawTraceStore,
// extracted so recordRawTrace's behavior (in particular, real vs. hardcoded
// header capture) can be pinned by a test without a live database. Mirrors
// pkg/server/proxy.go's identical seam over the same store.
type rawTraceRecorder interface {
	Insert(ctx context.Context, r rawtrace.RawHTTPRequest) error
}

type ClientOption func(*Client)

// WithProviders installs the provider registry. Required: without it there is
// no way to resolve a model id to a sender.
func WithProviders(s *providers.Set) ClientOption {
	return func(c *Client) { c.set = s }
}

// WithProviderSender overrides the sender for one provider name. Callers that
// want the registry to build its own senders do not use this; it exists for
// tests and for the --fake-turns seam.
func WithProviderSender(name string, s Sender) ClientOption {
	return func(c *Client) { c.senders[name] = s }
}

// WithStore wires the Postgres pool for capture + DB-backed conversations.
// Required for Conversation; SendParams degrades to capture-less without it.
func WithStore(pool *pgxpool.Pool) ClientOption {
	return func(c *Client) { c.pool = pool }
}

// WithConvStore shares an existing conversation store (its cache and its
// backend) instead of building a private one. A daemon hands its one store to
// every client it builds, so a reader and a writer of the same conversation
// see one cache rather than one apiece.
//
// nil is a programming error, not "use the default": a caller with no store to
// share omits the option and the client builds its own (the standalone
// `rafikid agent` path). Passing nil would install nothing, and the client's
// private fallback would then be a SECOND cache over the same rows — exactly
// the split the shared store exists to prevent. It panics rather than being
// ignored because a ClientOption cannot return an error (capture.WithConv
// refuses a nil store the same way).
func WithConvStore(cs *convpkg.Store) ClientOption {
	if cs == nil {
		panic("llm: WithConvStore(nil)")
	}
	return func(c *Client) { c.convs = cs }
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
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

// WithDefaultModel sets the model used when a conversation doesn't specify
// one. There is no built-in default: without this option and with no
// per-conversation model, resolution errors rather than silently picking one.
func WithDefaultModel(m string) ClientOption {
	return func(c *Client) { c.defaultModel = m }
}

// WithRouting carries the resolved routing spec (routing.Spec — the bracketed
// tail of the model string, or a routing policy row) for every OpenRouter
// request this client sends: the request body's provider object is built from
// spec.Prefs instead of the pin/guard logic alone. The zero spec (the
// default) leaves every body byte-identical to the pre-spec client.
func WithRouting(spec routing.Spec) ClientOption {
	return func(c *Client) { c.routingSpec = spec }
}

// WithRoutingSource installs a live routing-spec source, evaluated once per
// request, that takes precedence over WithRouting's static spec. It is how a
// fundi child running in-process reads its CURRENT stored spec on every send:
// an operator steer (Controller.SetChildRouting) rewrites the stored value and
// the child's next request sees it — the request in flight keeps the spec it
// started with, because spec() is called exactly once per request and the
// result threaded through the body build. A nil fn is refused: the option is a
// no-op, leaving the client on its static spec (and the zero spec by default).
func WithRoutingSource(fn func() routing.Spec) ClientOption {
	return func(c *Client) {
		if fn != nil {
			c.routingSource = fn
		}
	}
}

// spec resolves the routing spec for ONE request. It is the only reader of
// routingSpec (or routingSource) in the request path: every send calls it once
// at the top of prepareSend and threads the result through, so a steer landing
// mid-request cannot make one request's provider object and its error
// decoration disagree — it applies to the next request.
func (c *Client) spec() routing.Spec {
	if c.routingSource != nil {
		return c.routingSource()
	}
	return c.routingSpec
}

// WithRecordRequests enables raw HTTP request/response capture to the debug
// raw_http_request hypertable. Pass nil to disable (the default). The nil
// check happens here, not just at the call site: c.rawTrace is an interface
// (rawTraceRecorder) so tests can fake it, and assigning a nil
// *rawtrace.RawTraceStore straight into an interface field would produce a
// non-nil interface wrapping a nil pointer — recordRawTrace's own `c.rawTrace
// == nil` guard would then never fire.
func WithRecordRequests(s *rawtrace.RawTraceStore) ClientOption {
	return func(c *Client) {
		if s != nil {
			c.rawTrace = s
		}
	}
}

// WithTracerProvider injects OpenTelemetry tracing. The library never
// installs a global provider: embedded mode passes the host's provider,
// the standalone binary constructs one from OTLP env vars. Omitted = no-op.
func WithTracerProvider(tp trace.TracerProvider) ClientOption {
	return func(c *Client) { c.tracer = tp.Tracer("go.graveland.dev/rafiki/pkg/llm") }
}

// WithSpanParent sets where this client's spans attach when the caller's
// context carries no span: a fundi child's LLM calls nest under the turn the
// daemon's span observer holds open. Ignored when ctx already has a valid span.
func WithSpanParent(f func() trace.SpanContext) ClientOption {
	return func(c *Client) { c.spanParent = f }
}

// spanCtx returns ctx with the configured span parent attached when ctx
// carries no span of its own. An invalid returned SpanContext is ignored, so a
// parent source that has not opened its span yet leaves the call a root.
func (c *Client) spanCtx(ctx context.Context) context.Context {
	if c.spanParent == nil || trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	if sc := c.spanParent(); sc.IsValid() {
		return trace.ContextWithSpanContext(ctx, sc)
	}
	return ctx
}

func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		senders:  map[string]Sender{},
		breakers: map[string]*routing.Breaker{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.set == nil {
		c.set = providers.Default()
	}
	if c.logger == nil {
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if c.tracer == nil {
		c.tracer = noop.NewTracerProvider().Tracer("go.graveland.dev/rafiki/pkg/llm")
	}
	if c.catalog == nil {
		c.catalog = routing.NewModelCatalog(http.DefaultClient, time.Hour, c.logger)
	}
	// Build a sender for every configured provider that does not already have
	// one injected. A provider whose sender cannot be constructed (a reserved
	// kind, say) is skipped rather than fatal: the process may never use it,
	// and callModel's "provider not configured" error names it precisely if
	// something does.
	//
	// A kind with no key-based sender by design (openai-responses-chatgpt,
	// bound per owner at spawn) is not a fault and is not reported as one:
	// warning "provider unavailable" at every boot for a provider that works
	// sends the operator looking for a bug that is not there.
	for _, name := range c.set.Names() {
		if c.senders[name] != nil {
			continue
		}
		s, err := SenderFor(c.set.Providers[name], nil)
		if err != nil {
			if errors.Is(err, ErrNeedsOwnerSender) {
				c.logger.Debug(
					"llm: provider has no key-based sender; it is bound per owner at spawn",
					"provider", name)
				continue
			}
			c.logger.Warn("llm: provider unavailable", "provider", name, "error", err)
			continue
		}
		c.senders[name] = s
	}
	if c.breakerWindow > 0 {
		for u := range c.senders {
			c.breakers[u] = routing.NewBreaker(c.breakerWindow)
		}
	}
	if c.pool != nil {
		c.capture = capture.NewCaptureStore(c.pool)
	}
	// No option is the ONLY way to reach this private store: WithConvStore
	// refuses nil, so a caller either shares its store or builds one here —
	// never a nil "meaning default" that would slip a second cache in later.
	if c.convs == nil {
		if c.pool != nil {
			c.convs = convpkg.NewStore(store.NewConvBackend(c.pool), convpkg.Options{})
		} else {
			c.mem = convpkg.NewMemBackend()
			c.convs = convpkg.NewStore(c.mem, convpkg.Options{})
		}
	}
	// The capture store shares this client's conversation store — the caller's
	// when WithConvStore set one, this client's private default otherwise — so a
	// node written through capture is cached once, for the same reader that
	// replays it. Attached AFTER the default above, which is what keeps the
	// no-option path at one store too.
	if c.capture != nil {
		c.capture = c.capture.WithConv(c.convs)
	}
	c.modelGate = NewModelGate(10*time.Second, 300*time.Second)
	return c, nil
}

// SetLease installs the conversation write lease on this client's stores, so
// every message append is fenced by it.
//
// Called exactly once, at engine build, after the conversation is resolved and
// before any turn can run. One Client is built per child (see BuildEngine), so
// there is no concurrency here and nothing has written yet. The lease reaches a
// conversation through Conversation.handle, which resolves it on every call
// rather than capturing it at Open time — that is what lets this land after
// Conversation() returned.
//
// A zero lease, or a client with no stores, is a no-op — that is the unfenced
// behaviour the proxy face and every client-driven conversation rely on.
func (c *Client) SetLease(l store.Lease) {
	if !l.Held() || c.capture == nil || c.convs == nil {
		return
	}
	c.lease = l
	c.capture = c.capture.WithLease(l)
}

// Breaker exposes the breaker for a provider so the embedded proxy face can
// share it (one health signal per provider per process). Nil when breakers
// are disabled or the provider isn't configured.
func (c *Client) Breaker(name string) *routing.Breaker { return c.breakers[name] }

// Catalog exposes the model catalog (shared with the proxy face).
func (c *Client) Catalog() *routing.ModelCatalog { return c.catalog }

// Pool exposes the store pool for host recovery sweeps
// (store.UnfinishedConversations). Nil without WithStore.
func (c *Client) Pool() *pgxpool.Pool { return c.pool }

// ConversationCost returns the total list-price cost of a conversation's
// completed turns so far, each priced at its own model's rate. It is the
// fundi-side mirror of the proxy's cost rollup (pkg/server/proxy.go
// costFields) so the two "cost_total" log fields mean the same thing: the
// lifetime spend on this conversation, not the running cost of the current
// turn.
//
// A conversation can change model mid-flight (a failover to OpenRouter, a
// caller switching lines), so prior turns are priced per model via
// ConversationTokens rather than summed flat and priced once. Returns 0 when
// there is no capture store or the conversation id is empty; a rollup failure
// is indistinguishable from "no prior turns" and also returns 0 — the caller
// treats the field as best-effort, exactly as the proxy does.
func (c *Client) ConversationCost(ctx context.Context, convID string) float64 {
	if c.capture == nil || convID == "" {
		return 0
	}
	prior, err := c.capture.ConversationTokens(ctx, convID)
	if err != nil {
		return 0
	}
	var total float64
	for _, m := range prior {
		mp, ok := c.turnPricing(m.Model)
		if !ok {
			continue
		}
		total += mp.CostOf(m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens).Total
	}
	return total
}

// turnPricing resolves a captured turn's model id to its list price.
//
// conversation_turn.model records the PROVIDER-LOCAL id (prepareSend rewrites
// params.Model to the model id providers.Set.Resolve returned, and the
// completed turn takes the upstream's reported model), so a Responses-kind
// provider's "gpt-6.1-sol" is stored bare while the catalog indexes
// "openai/gpt-6.1-sol". Pricing that row by its raw id misses, and a skipped
// row is the same number as a free one: it silently resets a child's lifetime
// maxCost guardrail to the current turn.
//
// The row carries no provider name, so the candidate ids come from the
// provider registry this client already holds (providers.Set.CatalogIDs) and
// the raw id is tried FIRST. Every non-Responses kind maps to the identity, so
// a catalog-shaped id prices exactly as it did before, and a conversation that
// failed over BETWEEN kinds still bills each turn at its own model's rate
// rather than at whatever the newest kind's id shape implies.
func (c *Client) turnPricing(model string) (routing.ModelPricing, bool) {
	for _, id := range c.set.CatalogIDs(model) {
		if mp, ok := c.catalog.Pricing(id); ok {
			return mp, true
		}
	}
	return routing.ModelPricing{}, false
}

// SetProviderGuard attaches the provider cache guard, which ejects an
// OpenRouter provider from routing after it stops serving prompt-cache hits.
// Pass nil to disable; a nil guard is inert at every call site.
func (c *Client) SetProviderGuard(g *routing.ProviderGuard) { c.guard = g }

// ProviderSlug returns the OpenRouter routing slug for a provider display name
// as ProviderOf reports it — the name `rafiki providers ban` takes. Nil-safe.
func (c *Client) ProviderSlug(name string) string {
	if c == nil {
		return routing.GuessProviderSlug(name)
	}
	return c.guard.Slug(name)
}

// WithProviderGuard is SetProviderGuard as a construction option, for hosts
// that assemble a ClientOption list (pkg/fundi's in-process children share the
// daemon's one guard this way).
func WithProviderGuard(g *routing.ProviderGuard) ClientOption {
	return func(c *Client) { c.guard = g }
}

// SendMeta carries per-send capture attribution and routing selection, used
// by hosts that build their own params (sc's core-dump analyzer) and
// internally by Conversation.Send.
type SendMeta struct {
	ConversationID   string
	OriginEntrypoint string
	DrivenBy         store.DrivenBy
	OwnerUserID      string // conversations.users.id; empty → unattributed
	Persona          string
	Name             string
	ExternalRef      string
	Ordinal          int
	Source           string
	AuthorUserID     string // conversations.users.id; empty → unattributed
	AuthorKind       string
	RateLimit        RateLimitPolicy

	// OnBatchWait, when non-nil, is called with true just before a send parks
	// (a :batch first call handed to the Batcher) and with false when it
	// returns — on every path, including errors and ctx cancellation.
	OnBatchWait func(waiting bool)

	Primary  string   // empty = the provider named by the model id
	Fallback []string // nil = the provider's configured chain; empty non-nil = no failover

	// RequestHeadID is the conversation_message id of the last stored row this
	// request covered — the turn's head. It feeds the turn row's
	// request_head_id, which names the content the request sent instead of
	// re-storing the body. Empty (in-memory conversations, whose row ids are
	// synthetic) records NULL.
	RequestHeadID string

	// TrimFrom and TrimTo are the history positions a trim dropped from this
	// request: TrimFrom is the first dropped position (where the anchor message
	// ends) and TrimTo one past the last. Both nil when nothing was trimmed.
	// They feed the turn row's request_trim_from/request_trim_to.
	TrimFrom, TrimTo *int
}

// SendParams is the SDK-typed entry point: it converts params to a native
// request, sends it, and converts the reply back. Hosts that build their own
// params (sc's core-dump analyzer, the provider-guard config test) use it;
// every conversation send calls send directly with a native request.
func (c *Client) SendParams(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	req, extra, err := msganthropic.RequestFromSDK(params)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, meta, req, extra)
	if err != nil {
		return nil, err
	}
	return msganthropic.ToSDKMessage(resp)
}

// send write-aheads the turn, routes the call through the primary's breaker
// with fallback, and completes/fails the turn. Capture is best-effort: a broken
// store degrades to pass-through, never blocks the call. Every invocation
// inserts its own turn row and always resolves it.
//
// extra is the caller's top-level body fields (an SDK params' ExtraFields, for
// the SendParams face); prepareSend merges the provider object and the
// provider's configured extras onto a copy of it.
func (c *Client) send(ctx context.Context, meta SendMeta, req *msg.Request, extra map[string]any) (*msg.Response, error) {
	p, err := c.prepareSend(ctx, meta, req, extra)
	if err != nil {
		return nil, err
	}
	ctx, span := p.ctx, p.span
	defer span.End()

	if IsBatchModel(p.req.Model) {
		return c.parkSend(ctx, span, meta, p)
	}

	if err := c.modelGate.beforeSend(ctx, p.req.Model); err != nil {
		return nil, err
	}

	ref := c.beginTurn(ctx, meta, p.req, p.primary)
	ctx = WithSessionID(ctx, ref.convID)
	ctx, hdrs := withRawTraceHeaders(ctx)

	start := time.Now()
	resp, servedBy, err := c.callModel(ctx, span, p.primary, p.fallbacks, p.req, p.extra, p.spec)
	latency := int(time.Since(start).Milliseconds())
	span.SetAttributes(attribute.String("rafiki.upstream", servedBy))

	if err != nil {
		span.RecordError(errors.New(telemetry.ErrorText(err.Error())))
		span.SetStatus(codes.Error, "")
		c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, err)
		// Raw trace: non-streaming error path.
		c.recordRawTrace(ctx, meta, ref.turnID, p.req, nil, 0, p.primary, latency, err, hdrs)
		return nil, err
	}

	setUsageAttrs(span, resp)
	c.observeProvider(ref, p.req.Model, servedBy, resp)
	warnNewExt(c.logger, resp)

	if ref.capturing {
		c.completeTurn(ctx, ref.turnID, ref.createdAt, resp, servedBy, latency)
	}
	// Raw trace: non-streaming success path.
	c.recordRawTrace(ctx, meta, ref.turnID, p.req, resp, 200, servedBy, latency, nil, hdrs)
	return resp, nil
}

// setUsageAttrs records a response's model and token counts on the span. Every
// send path uses it, so the attribute keys and the nil-as-zero reading cannot
// drift between them.
func setUsageAttrs(span trace.Span, resp *msg.Response) {
	u := resp.Usage
	span.SetAttributes(
		attribute.Int64("rafiki.tokens.input", msg.Val(u.Input)),
		attribute.Int64("rafiki.tokens.output", msg.Val(u.Output)),
		attribute.Int64("rafiki.tokens.cache_read", msg.Val(u.CacheRead)),
		attribute.Int64("rafiki.tokens.cache_creation", msg.Val(u.CacheWrite)),
		attribute.String("gen_ai.response.model", resp.Model),
		attribute.Int64("gen_ai.usage.input_tokens", msg.Val(u.Input)),
		attribute.Int64("gen_ai.usage.output_tokens", msg.Val(u.Output)),
		attribute.Int64("gen_ai.usage.cache_read.input_tokens", msg.Val(u.CacheRead)),
		attribute.Int64("gen_ai.usage.cache_creation.input_tokens", msg.Val(u.CacheWrite)),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{resp.StopReason.String()}),
	)
}

// observeProvider feeds the provider cache guard — but only with evidence. An
// OpenRouter turn that reports neither a cache-read nor an input count says
// nothing about the backend's cache behaviour, and a fabricated observation
// would eject a healthy provider. The gate is the guard's own input contract,
// not a heuristic: both counts must be present.
func (c *Client) observeProvider(ref turnRef, model, servedBy string, resp *msg.Response) {
	p, ok := c.set.Get(servedBy)
	if !ok || p.Kind != providers.KindAnthropicOpenRouter {
		return
	}
	if resp.Usage.CacheRead == nil || resp.Usage.Input == nil {
		return
	}
	c.guard.Observe(time.Now(), routing.Observation{
		Provider: ProviderOfMsg(resp), Model: model, Conversation: ref.convID,
		PrefixHash: ref.prefixHash, InputTokens: msg.Val(resp.Usage.Input),
		CacheReadTokens: msg.Val(resp.Usage.CacheRead),
	})
}

// preparedSend is prepareSend's answer: everything a send needs after routing
// has been decided once, so no later stage re-derives any of it.
type preparedSend struct {
	ctx       context.Context
	span      trace.Span
	primary   string
	fallbacks []string
	batchOnly []string
	req       *msg.Request
	spec      routing.Spec
	extra     map[string]any
}

// prepareSend is the ONE place that decides how a request is routed. Every
// send path (send, sendStreamingAttempt) calls it first and MUST NOT re-derive
// any of this itself — a second copy would silently drift, e.g. streaming
// requests reaching a different provider than non-streaming ones for the same
// model id. It:
//
//  1. Resolves req.Model ("<provider>/<model>", or a bare id against
//     default_provider) to a provider and a provider-local model id, and
//     rewrites req.Model to the local id.
//  2. Picks the fallback chain: the caller's when they supplied one (including
//     an explicitly empty one, which means "no failover"), the provider's
//     configured chain otherwise.
//  3. Applies the kind's request mutation — for anthropic-openrouter that is
//     the provider preferences and the cache guard's ejections — BEFORE
//     capture, so the recorded request matches the wire.
//  4. Opens the "llm.send" tracing span and records breaker state.
//  5. Applies the :batch routing rule: a model id ending in ":batch" is only
//     ever served by an anthropic-openrouter provider (anything else is an
//     error, never a live send), and the suffix survives into req.Model
//     exactly when this send PARKS — a first call (no assistant-role message
//     in req.Messages) keeps ":batch", a later call has it stripped and
//     goes through the normal live sender.
//
// The resulting INVARIANT every later check reads instead of re-deriving the
// rule: **after prepareSend, req.Model ends in ":batch" iff this send
// parks.**
// The returned error is a resolution failure (unknown provider, empty model);
// callers must abort the send on it.
func (c *Client) prepareSend(ctx context.Context, meta SendMeta, in *msg.Request, extra map[string]any) (*preparedSend, error) {
	// Work on a copy: routing rewrites Model, and a caller's request must stay
	// the one it built — a streaming attempt that steps aside hands the SAME
	// request to the non-streaming path, which must resolve the original id
	// again rather than a provider-local one.
	req := new(msg.Request)
	*req = *in
	requested := req.Model
	if requested == "" {
		requested = c.defaultModel
	}
	// A :batch suffix is not part of a declared model's name: the parked
	// call's declaration lookup must see the BASE id (declaration keys are
	// exact-match, so "glm-flash:batch" matches nothing). Resolve against the
	// stripped id and re-apply the suffix below exactly when this send parks;
	// the park invariant is unchanged.
	//
	// batched is true when the suffix arrives on the REQUESTED string (a
	// literal …:batch) OR on the declaration-RESOLVED id: a declaration whose
	// id ends in :batch is a supported shape, and without the second arm
	// batched would be false, the strip below would never run, and EVERY turn
	// would park instead of only the first.
	batched := IsBatchModel(requested)
	p, modelID, decl, err := c.set.Resolve(strings.TrimSuffix(requested, BatchSuffix))
	if err != nil {
		return nil, err
	}
	batched = batched || IsBatchModel(modelID)
	// baseID is the suffix-free id every arm builds from: a declaration's raw ID
	// may itself end in :batch, so the first-call arm must STRIP before
	// re-appending (adding a suffix to a suffixed id would park
	// "…:batch:batch", a model no endpoint serves).
	baseID := strings.TrimSuffix(modelID, BatchSuffix)
	req.Model = modelID

	// The :batch routing rule, in one place. After this block: req.Model
	// ends in ":batch" iff this send parks (the invariant send,
	// sendStreamingAttempt and everything downstream rely on).
	if batched {
		if p.Kind != providers.KindAnthropicOpenRouter {
			return nil, fmt.Errorf("llm: model %q: %s models are served only by an anthropic-openrouter provider", requested, BatchSuffix)
		}
		if firstCall(req) {
			modelID = baseID + BatchSuffix
		} else {
			// Later calls go live on the base id, for every batched shape (the
			// literal case is stripped by construction; the declaration case
			// may have carried the suffix in its own id).
			modelID = baseID
		}
		req.Model = modelID
	}

	primary := p.Name
	fallbacks := p.Fallback
	if meta.Fallback != nil {
		fallbacks = meta.Fallback
	}
	if meta.Primary != "" {
		primary = meta.Primary
	}
	// Resolve the routing spec ONCE for this request and thread the local
	// through — the body build, the parked batch envelope's only-list, the
	// tracing attribute and the no-eligible error decoration all read THIS
	// value, so a steer landing mid-request applies to the NEXT request and
	// never makes one request's body and error disagree.
	spec := c.spec()
	extra = c.mutateParams(p, decl, req.Model, extra, spec)

	// The parked call's only-list, computed HERE while the declaration is in
	// hand — the same pinOnly rule applyProviderPrefs applies (spec Only >
	// declaration Only). nil = no pin: the batch envelope carries no provider
	// key at all.
	var batchOnly []string
	if batched {
		batchOnly = batchOnlyList(spec, decl, strings.TrimSuffix(modelID, BatchSuffix))
	}

	attrs := []attribute.KeyValue{
		attribute.String("rafiki.model", req.Model),
		attribute.String("rafiki.primary", primary),
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", req.Model),
		attribute.String("gen_ai.provider.name", primary),
	}
	if !spec.IsZero() {
		attrs = append(attrs, attribute.String("rafiki.routing", spec.String()))
	}
	if meta.OriginEntrypoint != "" {
		attrs = append(attrs, attribute.String("rafiki.entrypoint", meta.OriginEntrypoint))
	}
	sctx, span := c.tracer.Start(c.spanCtx(ctx), "chat "+req.Model,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	if b := c.breakers[primary]; b != nil {
		span.SetAttributes(attribute.Bool("rafiki.breaker.open", b.Open()))
	}
	return &preparedSend{
		ctx: sctx, span: span, primary: primary, fallbacks: fallbacks,
		batchOnly: batchOnly, req: req, spec: spec, extra: extra,
	}, nil
}

// mutateParams applies a kind's request-body mutation. Only
// anthropic-openrouter has one: OpenRouter's non-standard top-level "provider"
// field, carrying the declared routing preferences (a model declaration's own
// only pin, when the request was made through one), plus whatever the cache
// guard has ejected — and, when a routing spec is set on the client
// (WithRouting), the spec's own provider-routing decisions.
// providers.Validate refuses extras.provider, so the user cannot clobber it.
//
// decl is the ModelDecl the requested model id matched, from
// providers.Set.Resolve; nil for the fallback path (callModel re-resolves the
// model against the FALLBACK provider via the catalog, which knows nothing
// about the primary's declarations) and for undeclared ids.
//
// The result is the request's top-level extra fields, passed to the codec as
// msg.EncodeOptions.Extra; the caller's own fields are carried through.
func (c *Client) mutateParams(p providers.Provider, decl *providers.ModelDecl, model string, extra map[string]any, spec routing.Spec) map[string]any {
	if p.Kind != providers.KindAnthropicOpenRouter {
		return extra
	}
	return applyProviderPrefs(model, decl, c.guard, p.Extras, spec, extra)
}

// failTurn best-effort fails a captured turn; a no-op when capturing is
// false. Shared by SendParams and sendStreaming so the capture-failure log
// line stays in one place.
func (c *Client) failTurn(ctx context.Context, capturing bool, turnID string, turnCreatedAt time.Time, err error) {
	if !capturing {
		return
	}
	if ferr := c.capture.FailTurn(ctx, turnID, turnCreatedAt, err.Error()); ferr != nil {
		c.logger.Warn("capture: fail-turn write failed", "error", ferr)
	}
}

// sendStreaming issues req through the streaming path, invoking the sink once
// per event AS IT ARRIVES and accumulating the events into the returned
// response via msg.Accumulator.
//
// attempted reports whether the CALLER must fall back to send itself:
//   - false means the caller must call send — either no request was
//     sent at all here (the resolved sender has no streaming capability: no
//     turn row, no side effect), or a request WAS sent and failed, but
//     delivered is also false (see below), so send retrying the whole send —
//     including its full breaker/fallback chain — is safe.
//   - true means the result here is final: either it succeeded, or it
//     failed after already delivering at least one event, in which case a
//     retry is never safe and the caller must surface err directly.
//
// delivered reports whether AT LEAST ONE content-bearing event reached the
// sink during this call. This is the structural guard sendWithTrim's
// trim-retry loop (and, transitively, sendAttempt's own-package caller) relies
// on: once delivered is true, retrying — here or in send — is never safe,
// because a retry opens a brand new, independent request and the caller has no
// way to know the previous one already delivered partial output to the sink.
// This holds regardless of what the resulting error turns out to be; nothing
// here or in sendWithTrim assumes a particular failure shape delivers before
// content.
//
// Failover: sendStreaming DOES engage even when a fallback chain and an active
// breaker are configured — it never bails out of streaming entirely just
// because both are set. A concrete consumer (fundi) enables a fallback chain
// and WithBreaker together whenever an OpenRouter key is configured, which is
// the common case, not an edge case; bailing out of streaming there made
// WithStreamHandlerMsg silently a no-op for exactly the deployment this was
// built for.
//
// Breaker gate AND result recording both genuinely apply here, via the same
// primaryGate/recordPrimaryResult helpers callModel itself uses:
//   - Before issuing anything, primaryGate(primary, fallbacks, now) is
//     consulted. When it says NOT to use primary (breaker open, no probe
//     slot available), sendStreaming issues NOTHING — no request, no turn
//     row — and reports attempted=false so the caller's send call runs the
//     complete breaker-gated fallback chain non-streamed, exactly the handoff
//     shape already used when the resolved sender lacks streaming capability
//     at all.
//   - When primaryGate does say to use primary, the returned breaker (nil
//     when bypassed — no fallback configured, or breakers disabled) is used
//     to recordPrimaryResult once the attempt resolves: success closes it, a
//     retryable failure trips/extends it, a non-retryable failure is left
//     alone — the identical rule callModel applies to its own sender call.
//     EXCEPTION: recording is skipped exactly on the two pre-delivery hand-off
//     paths below (breaker != nil, nothing delivered yet) — the caller's own
//     follow-up callModel attempt is what records there, and it runs moments
//     later. Recording in both places would double-record one logical failure
//     and pre-emptively trip the breaker before that retry ever gets a chance
//     to run — this is not an oversight, it is enforced per-branch below.
//   - breaker is ALSO still used to decide what a PRE-delivery failure
//     (delivered still false) should report:
//   - breaker != nil: nothing has reached the sink yet, so nothing can be
//     double-delivered by a retry. sendStreaming reports attempted=false; the
//     caller's subsequent send call re-tries primary (subject to the SAME
//     breaker gate) and, on a retryable failure, runs the complete
//     breaker-gated fallback chain non-streamed.
//   - breaker == nil (no fallback configured, or no breaker — the same shape
//     callModel itself would call directly with no failover to offer):
//     bouncing to send would only re-issue to the identical primary sender,
//     wasting a request. sendStreaming instead reports attempted=true so the
//     caller's own retry logic (sendWithTrim's trim-retry loop) can re-attempt
//     via a fresh streaming call.
//
// A MID-stream failure (delivered already true) is never safe to hand off
// regardless of breaker — see above — so sendStreaming always reports
// attempted=true there and the error surfaces directly, with no failover.
//
// Turn bookkeeping on a breaker-eligible pre-delivery handoff: this method
// still calls beginTurn before issuing the request and failTurn on the
// resulting error, so the streaming attempt's own turn row is written and
// correctly marked failed, even though attempted=false. The caller's subsequent
// send call then begins and resolves ITS OWN, separate turn row for its own
// (possibly multi-upstream, via callModel) request. This is not an orphan or a
// duplicate: it is the same one-row-per-invocation convention sendWithTrim's
// own trim-retry loop already relies on (each retry attempt is its own send
// call and so its own turn row) — two real requests were made here (one
// streamed and rejected, one non-streamed and resolved), so two rows accurately
// reflect that.
func (c *Client) sendStreaming(ctx context.Context, meta SendMeta, req *msg.Request, handler streamSink) (*msg.Response, bool, bool, error) {
	rlp := meta.RateLimit.effective()
	model := req.Model

	for attempt := 0; attempt <= rlp.MaxRetries; attempt++ {
		resp, attempted, delivered, err := c.sendStreamingAttempt(ctx, meta, req, handler)
		if err == nil || !attempted {
			return resp, attempted, delivered, err
		}
		// A streaming error with delivered==true is never retryable.
		if delivered {
			return resp, attempted, delivered, err
		}
		isRL, retryAfter := isRateLimit(err)
		if !isRL && IsRateLimitStreamError(err) {
			// The same 429 arriving as an in-band SSE "error" event — an upstream
			// provider rejected the request after the connection came up. No
			// Retry-After exists inside a stream payload, so retryAfter stays 0
			// and the ModelGate's exponential backoff applies.
			isRL = true
		}
		if !isRL || attempt >= rlp.MaxRetries {
			return resp, attempted, delivered, err
		}
		c.modelGate.record429(model, retryAfter)
		c.logger.Warn("rate limit hit; retrying after backoff",
			"model", model, "attempt", attempt+1, "max", rlp.MaxRetries)
	}
	return nil, true, false, fmt.Errorf("llm: rate limit retries exhausted for model %s", model)
}

func (c *Client) sendStreamingAttempt(ctx context.Context, meta SendMeta, req *msg.Request, handler streamSink) (resp *msg.Response, attempted bool, delivered bool, err error) {
	p, err := c.prepareSend(ctx, meta, req, nil)
	if err != nil {
		return nil, false, false, err
	}
	ctx, span := p.ctx, p.span
	defer span.End()

	// The prepareSend park invariant: a ":batch" model here means this send
	// parks. Step aside BEFORE beginTurn — no turn row, no stream opened — so
	// the caller falls through to send, which owns the batch path.
	if IsBatchModel(p.req.Model) {
		return nil, false, false, nil
	}

	if err := c.modelGate.beforeSend(ctx, p.req.Model); err != nil {
		return nil, false, false, err
	}

	sender := c.senders[p.primary]
	if sender == nil {
		// No sender for this provider: step aside without a request, so the
		// caller's send call reports "provider not configured" precisely.
		return nil, false, false, nil
	}

	now := time.Now()
	usePrimary, breaker := c.primaryGate(p.primary, p.fallbacks, now)
	if !usePrimary {
		// The breaker is open and holds no probe slot right now: the primary
		// is known-bad. Issue nothing at all — no request, no turn row, the
		// same shape as the capability-bypass case above — so the caller's
		// send call runs the complete breaker-gated fallback chain
		// non-streamed instead of burning a full timeout on a dead primary.
		c.logger.Warn("breaker open; stepping aside from streaming primary", "primary", p.primary)
		return nil, false, false, nil
	}

	ref := c.beginTurn(ctx, meta, p.req, p.primary)
	ctx = WithSessionID(ctx, ref.convID)
	ctx, hdrs := withRawTraceHeaders(ctx)
	start := time.Now()

	sessionID, _ := sessionIDFromContext(ctx)
	dec, serr := sender.Stream(ctx, p.req, msg.EncodeOptions{SessionID: sessionID, Extra: p.extra})
	// The defer is registered BEFORE the error checks below, and guarded on
	// dec != nil rather than serr == nil: Sender's contract does not forbid an
	// implementation from returning (non-nil decoder, non-nil error).
	// Registering the defer only after those checks would leak that decoder and
	// its response body on exactly that path — this is what
	// TestSendStreaming_ClosesStreamEvenWhenNewStreamingReturnsBothStreamAndError
	// pins down.
	if dec != nil {
		defer func() {
			if cerr := dec.Close(); cerr != nil {
				c.logger.Warn("llm: close stream failed", "error", cerr)
			}
		}()
	}
	if serr != nil {
		span.RecordError(errors.New(telemetry.ErrorText(serr.Error())))
		span.SetStatus(codes.Error, "")
		c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, serr)
		if breaker != nil {
			// Nothing delivered yet and a real fallback exists: hand off
			// WITHOUT recording here. send's callModel is about to make its
			// OWN authoritative attempt against primary and will record THAT
			// result; recording this pre-delivery failure too would
			// pre-emptively trip the breaker and make callModel skip the very
			// retry this handoff exists to give it.
			return nil, false, false, serr
		}
		// No fallback to offer: breaker is nil here (primaryGate's bypass
		// case), so there's nothing to record — report attempted=true so
		// sendWithTrim's own retry loop (not send) re-attempts via streaming
		// again, instead of wastefully re-issuing to the same primary sender.
		c.recordRawTrace(ctx, meta, ref.turnID, p.req, nil, 0, p.primary, int(time.Since(start).Milliseconds()), serr, hdrs)
		// The breaker==nil branch is production's shape (daemon children run
		// with no fallback chain), so THIS is the return a no-eligible routing
		// rejection surfaces through on the streaming path — decorate it here,
		// or decision 6's "error naming the spec and the model" only ever fired
		// for non-streamed sends. The trace and turn rows above keep the raw
		// upstream error; the decoration is for the caller.
		return nil, true, false, c.noEligibleError(p.spec, p.req.Model, serr)
	}

	acc := msg.NewAccumulator()
	// The SDK-typed sink gets one adapter per stream: it replays, at block
	// stop, whatever a finalized block carries that the stream never delivered
	// as a delta (a citation list, a server-tool block's completed input).
	var adapter *msganthropic.SDKEventAdapter
	if handler.sdk != nil {
		adapter = &msganthropic.SDKEventAdapter{}
	}
	for serr == nil {
		ev, derr := dec.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			serr = derr
			break
		}
		// Only content-bearing events count as delivered. message_start
		// carries no text, so treating it as delivery would make the failover
		// branch below unreachable for the common "connected, then died before
		// any output" failure — which the non-streaming path recovers from.
		content := msg.DeliversContent(ev)
		if content {
			delivered = true
		}
		if aerr := acc.Add(ev); aerr != nil {
			wrapped := fmt.Errorf("llm: accumulate stream event: %w", aerr)
			span.RecordError(errors.New(telemetry.ErrorText(wrapped.Error())))
			span.SetStatus(codes.Error, "")
			c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, wrapped)
			recordPrimaryResult(breaker, now, wrapped)
			c.recordRawTrace(ctx, meta, ref.turnID, p.req, nil, 0, p.primary, int(time.Since(start).Milliseconds()), wrapped, hdrs)
			return nil, true, delivered, wrapped
		}
		if adapter != nil {
			sdkEvents, aerr := adapter.Add(ev)
			if aerr != nil {
				wrapped := fmt.Errorf("llm: convert stream event: %w", aerr)
				span.RecordError(errors.New(telemetry.ErrorText(wrapped.Error())))
				span.SetStatus(codes.Error, "")
				c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, wrapped)
				recordPrimaryResult(breaker, now, wrapped)
				c.recordRawTrace(ctx, meta, ref.turnID, p.req, nil, 0, p.primary, int(time.Since(start).Milliseconds()), wrapped, hdrs)
				return nil, true, delivered, wrapped
			}
			for _, sdkEv := range sdkEvents {
				FixEmptyToolInput(&sdkEv)
				handler.sdk(sdkEv)
			}
		}
		if content && handler.native != nil {
			handler.native(acc.Response())
		}
	}
	if serr != nil {
		span.RecordError(errors.New(telemetry.ErrorText(serr.Error())))
		span.SetStatus(codes.Error, "")
		c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, serr)
		if !delivered && breaker != nil {
			// Hand off WITHOUT recording, for the same reason as the
			// Stream-error branch above: callModel's own imminent
			// primary retry is the authoritative attempt and will record its
			// own result. Recording this one too would trip the breaker
			// before that retry runs at all.
			return nil, false, false, serr
		}
		// Either something was already delivered (no handoff is possible
		// regardless of breaker state) or breaker is nil (bypass, no handoff
		// exists to defer to): this IS the final result, so record it.
		recordPrimaryResult(breaker, now, serr)
		c.recordRawTrace(ctx, meta, ref.turnID, p.req, nil, 0, p.primary, int(time.Since(start).Milliseconds()), serr, hdrs)
		// The breaker==nil !delivered shape is production's child-send path,
		// and a codec surfaces a pre-delivery HTTP failure through the STREAM
		// (Stream returns a decoder; the error arrives on the first Next) — so
		// THIS is where a no-eligible routing rejection decorates for streamed
		// sends. The trace keeps the raw error.
		return nil, true, delivered, c.noEligibleError(p.spec, p.req.Model, serr)
	}

	recordPrimaryResult(breaker, now, nil)
	c.modelGate.recordSuccess(p.req.Model)
	latency := int(time.Since(start).Milliseconds())
	// acc is the fully accumulated response at this point (the loop above has
	// already run to completion) — this is a completed turn's final usage, not
	// a partial mid-stream snapshot.
	resp = acc.Response()
	span.SetAttributes(attribute.String("rafiki.upstream", p.primary))
	setUsageAttrs(span, resp)
	c.observeProvider(ref, p.req.Model, p.primary, resp)
	warnNewExt(c.logger, resp)
	if ref.capturing {
		c.completeTurn(ctx, ref.turnID, ref.createdAt, resp, p.primary, latency)
	}
	c.recordRawTrace(ctx, meta, ref.turnID, p.req, resp, 200, p.primary, latency, nil, hdrs)
	return resp, true, delivered, nil
}

// primaryGate is the ONE place that decides whether a send should issue to
// the primary right now. Both callModel and sendStreaming MUST consult it
// (and only it) instead of separately reasoning about c.breakers[primary] —
// two independent copies of this decision drifting apart is exactly the bug
// class this fixes (C3: streaming read the breaker only to compute a
// display-only bool, never the live decision).
//
// A send with NO fallback configured — or breakers disabled entirely —
// bypasses the breaker altogether: primary is always used and breaker is nil
// (so the caller knows not to record a result either). This mirrors the
// routing core's fallback-less behavior; per the design, an empty Fallback
// chain is how a consumer opts out of being pinned.
//
// Otherwise the breaker's own live state is consulted via UsePrimary(now) —
// NOT whether a breaker merely exists — so an open breaker with no probe slot
// available correctly says "don't use primary" rather than always issuing
// because one is configured.
func (c *Client) primaryGate(primary string, fallbacks []string, now time.Time) (usePrimary bool, breaker *routing.Breaker) {
	b := c.breakers[primary]
	if len(fallbacks) == 0 || b == nil {
		return true, nil
	}
	return b.UsePrimary(now), b
}

// recordPrimaryResult mirrors callModel's post-attempt breaker bookkeeping so
// both the non-streaming and streaming send paths learn from the SAME rule:
// success closes an open breaker; a failover-worthy failure (retryable, or an
// out-of-credit account) trips/extends it; any other failure (bad auth,
// malformed request) is left alone since it says nothing about whether the
// primary itself is healthy. No-op when breaker is nil (the bypass case from
// primaryGate).
func recordPrimaryResult(breaker *routing.Breaker, now time.Time, err error) {
	if breaker == nil {
		return
	}
	if err == nil {
		breaker.RecordResult(now, false)
		return
	}
	if routing.FailoverWorthy(err) {
		breaker.RecordResult(now, true)
	}
}

// recordModelResult updates the model gate from a callModel result.
func (c *Client) recordModelResult(req *msg.Request, err error) {
	if err == nil {
		c.modelGate.recordSuccess(req.Model)
		return
	}
	if isRL, retryAfter := isRateLimit(err); isRL {
		c.modelGate.record429(req.Model, retryAfter)
	}
}

// callSender issues one request through s. The session id rides EncodeOptions
// as well as the context: the Anthropic kinds send it as a header through
// sessionIDTransport, and the Responses codec puts it in the request body's
// prompt_cache_key.
func callSender(ctx context.Context, s Sender, req *msg.Request, extra map[string]any) (*msg.Response, error) {
	sessionID, _ := sessionIDFromContext(ctx)
	return s.Send(ctx, req, msg.EncodeOptions{SessionID: sessionID, Extra: extra})
}

// callModel routes primary-with-breaker then the fallback chain. Fallback
// sends rewrite the model via the catalog (Anthropic id → OpenRouter id when
// the fallback is OpenRouter). A send with NO fallback configured bypasses
// the breaker entirely (direct primary, mirroring the routing core's
// fallback-less behavior) — per the design, an empty Fallback chain is how a
// consumer opts out of being pinned.
func (c *Client) callModel(ctx context.Context, span trace.Span, primary string, fallbacks []string, req *msg.Request, extra map[string]any, spec routing.Spec) (*msg.Response, string, error) {
	sender := c.senders[primary]
	if sender == nil {
		return nil, primary, fmt.Errorf("llm: provider %q not configured", primary)
	}
	now := time.Now()
	usePrimary, breaker := c.primaryGate(primary, fallbacks, now)

	if breaker == nil {
		resp, err := callSender(ctx, sender, req, extra)
		c.recordModelResult(req, err)
		return resp, primary, c.noEligibleError(spec, req.Model, err)
	}

	if usePrimary {
		resp, err := callSender(ctx, sender, req, extra)
		recordPrimaryResult(breaker, now, err)
		c.recordModelResult(req, err)
		if err == nil {
			return resp, primary, nil
		}
		if !routing.FailoverWorthy(err) {
			// not failover-worthy: don't fail over or trip. The no-eligible
			// wrap below is decoration only (%w), so this classification
			// sees exactly the error the sender returned.
			return nil, primary, c.noEligibleError(spec, req.Model, err)
		}
		span.AddEvent("failover", trace.WithAttributes(attribute.String("rafiki.error", telemetry.ErrorText(err.Error()))))
		c.logger.Warn("primary failed; failing over", "primary", primary, "error", err)
	}

	var lastErr error
	for _, fb := range fallbacks {
		fbSender := c.senders[fb]
		if fbSender == nil {
			lastErr = fmt.Errorf("llm: fallback provider %q not configured", fb)
			continue
		}
		fbProvider, ok := c.set.Get(fb)
		if !ok {
			lastErr = fmt.Errorf("llm: fallback provider %q not in the registry", fb)
			continue
		}
		// A shallow copy: the fallback's model is a translation of the
		// primary's, and the primary's request must stay the one it was.
		fbReq := *req
		// Model-id translation between a primary and its fallback is the
		// RECEIVING kind's job: anthropic-openrouter maps an Anthropic id
		// through the catalog; an anthropic -> anthropic fallback needs none.
		if fbProvider.Kind == providers.KindAnthropicOpenRouter {
			fbReq.Model = c.catalog.OpenRouterModel(req.Model)
		}
		// The fallback path has no declaration: the model was re-resolved
		// against the FALLBACK provider via the catalog, which knows nothing
		// about the primary's declared models, so nil means only the
		// fallback's own guard ejections apply, never a stale declaration pin.
		fbExtra := c.mutateParams(fbProvider, nil, fbReq.Model, extra, spec)
		resp, err := callSender(ctx, fbSender, &fbReq, fbExtra)
		if err == nil {
			c.modelGate.recordSuccess(req.Model)
			return resp, fb, nil
		}
		// Wrapped with the fallback's own (translated) model id, so the
		// message names the id OpenRouter actually rejected.
		lastErr = c.noEligibleError(spec, fbReq.Model, err)
	}
	return nil, primary, lastErr
}

// applyProviderPrefs injects OpenRouter provider-routing preferences as the
// request body's "provider" field and merges the provider's configured extras
// alongside. Which preferences depends on spec, the resolved routing spec
// (routing.Spec, zero = none):
//
//   - Zero spec — the pre-spec path (applyProviderPrefsPreSpec) runs: the
//     declaration's own Only when non-empty, plus any providers the guard has
//     ejected. Keeping that path separate is what guarantees a request with no
//     spec produces a byte-identical body to the pre-spec client.
//
//   - Non-zero spec — the spec's own decisions take over the provider object
//     through spec.Prefs: pinOnly is the declaration's Only when non-empty, and
//     the spec's own Only (an explicit operator decision about where the
//     request may be served) REPLACES it and bypasses the guard's bans and
//     ejections; sort/quantizations/data-policy ride along, the data flags even
//     under an only. prefs is sent only when spec.Prefs reports anything to
//     send.
//
// A declaration's Only pin, when non-empty, IS the pin for this request (two
// declarations may share one id and route it to different providers). The
// guard's Ignore list still merges in — a declaration pin declares who MAY
// serve; the guard vetoes who must not. With a declaration pin the body
// carries "provider" even when nothing has to be ignored.
//
// The returned map is the WHOLE extra set the codec merges into the body
// (msg.EncodeOptions.Extra), so everything that belongs in it must be
// assembled here in one call. "provider" is a reserved extras key
// (providers.Validate refuses it) precisely because a user-supplied one would
// otherwise delete the guard's ejections with no error and no log line.
func applyProviderPrefs(model string, decl *providers.ModelDecl, g *routing.ProviderGuard, extras map[string]any, spec routing.Spec, caller map[string]any) map[string]any {
	if spec.IsZero() {
		return applyProviderPrefsPreSpec(model, decl, g, extras, caller)
	}
	var pinOnly []string
	if decl != nil && len(decl.Only) > 0 {
		pinOnly = decl.Only
	}
	prefs, send := spec.Prefs(pinOnly, g.IgnoredFor(time.Now(), model))
	fields := make(map[string]any, len(extras)+len(caller)+1)
	for k, v := range caller {
		fields[k] = v
	}
	for k, v := range extras {
		fields[k] = v
	}
	if send {
		fields["provider"] = prefs
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// applyProviderPrefsPreSpec is the pre-routing-spec code path, run for a zero
// spec so no body byte can change for a request that carries no routing spec.
// Do not evolve it — new behaviour belongs in the spec branch of
// applyProviderPrefs. It sends a "provider" object only when a declaration
// pins providers or the guard has something to ignore; extras alone stay
// top-level.
func applyProviderPrefsPreSpec(model string, decl *providers.ModelDecl, g *routing.ProviderGuard, extras map[string]any, caller map[string]any) map[string]any {
	var prefs routing.ProviderPrefs
	pinned := false
	if decl != nil && len(decl.Only) > 0 {
		prefs.Only = decl.Only
		pinned = true
	}
	ignore := g.IgnoredFor(time.Now(), model)
	if !pinned && len(ignore) == 0 && len(extras) == 0 && len(caller) == 0 {
		return nil
	}
	fields := make(map[string]any, len(extras)+len(caller)+1)
	for k, v := range caller {
		fields[k] = v
	}
	for k, v := range extras {
		fields[k] = v
	}
	if pinned || len(ignore) > 0 {
		prefs.Ignore = append(prefs.Ignore, ignore...)
		fields["provider"] = prefs
	}
	return fields
}

// noEligibleUpstream reports whether err is OpenRouter's no-eligible-provider
// rejection: the class the Anthropic codec names ErrNoEligible — a 400/404
// whose body's error message is about endpoints or providers being unavailable,
// which is what a request whose routing spec (only, nodata, zdr, quantizations)
// excludes every host gets back. The class is deterministic — the same body
// fails identically forever — so it is never retried (a 400/404 is neither
// routing.Retryable nor agentloop-retryable) and the wrapper below only
// decorates it.
func noEligibleUpstream(err error) bool {
	var apiErr *msg.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Kind == msg.ErrNoEligible
}

// noEligibleError names the routing spec in OpenRouter's no-eligible-provider
// rejection, so the operator sees WHICH spec excluded every provider and not
// just OpenRouter's bare message. It wraps only when a non-zero spec is set
// on the client, and the wrap is decoration only: %w keeps the SDK error
// reachable, so FailoverWorthy and every retry classifier see exactly what
// they saw before (a 400/404 is not failover-worthy, and the fallback chain
// is unchanged).
func (c *Client) noEligibleError(spec routing.Spec, model string, err error) error {
	if err == nil || spec.IsZero() || !noEligibleUpstream(err) {
		return err
	}
	return fmt.Errorf("no provider can serve %s under routing [%s]: %w", model, spec.String(), err)
}

// ProviderOfMsg returns OpenRouter's non-standard top-level "provider" field —
// the backend that actually served the turn — or "" for a native Anthropic
// response, which carries none.
func ProviderOfMsg(r *msg.Response) string {
	if r == nil {
		return ""
	}
	return r.Provider
}

// ProviderOf is ProviderOfMsg for an SDK message. The Anthropic SDK has no
// such field, but it retains unknown members in JSON.ExtraFields, so the value
// survives the decode.
func ProviderOf(m *anthropic.Message) string {
	if m == nil {
		return ""
	}
	f, ok := m.JSON.ExtraFields["provider"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(f.Raw()), &s); err != nil {
		return ""
	}
	return s
}

// prefixEnvelope is the request's cacheable prefix — everything before the
// messages — in the canonical form nativePrefixHash digests. Field order is
// fixed by the struct, so two equal prefixes always marshal identically.
type prefixEnvelope struct {
	Model          string          `json:"model"`
	System         []string        `json:"system"`
	Tools          []msg.ToolDef   `json:"tools"`
	ForceTool      string          `json:"force_tool,omitempty"`
	MaxTokens      int64           `json:"max_tokens"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopK           *int64          `json:"top_k,omitempty"`
	ThinkingBudget int64           `json:"thinking_budget,omitempty"`
	Cache          msg.CachePolicy `json:"cache"`
}

// nativePrefixEnvelope builds req's prefix envelope: the system blocks as
// text, and every static request knob.
func nativePrefixEnvelope(req *msg.Request) prefixEnvelope {
	system := make([]string, 0, len(req.System))
	for _, s := range req.System {
		system = append(system, s.Text)
	}
	return prefixEnvelope{
		Model: req.Model, System: system, Tools: req.Tools, ForceTool: req.ForceTool,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopK: req.TopK,
		ThinkingBudget: req.ThinkingBudget, Cache: req.Cache,
	}
}

// nativePrefixHash digests the request's static prefix: the model, system,
// tools, sampling knobs and cache policy, with Messages left out. Two turns of
// one conversation share a prefix hash exactly when everything a provider
// caches is unchanged, and the provider guard reads that as its cache-miss
// evidence. An unencodable request hashes to "" — a value no captured turn
// shares, which fails the guard's comparison closed rather than open.
func nativePrefixHash(req *msg.Request) string {
	b, err := json.Marshal(nativePrefixEnvelope(req))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// turnRef is what a write-ahead turn leaves behind for the paths that resolve
// it: the ids to complete it with, plus the conversation and prefix hash the
// provider guard needs to judge a cache miss.
type turnRef struct {
	turnID     string
	createdAt  time.Time
	convID     string
	prefixHash string
	capturing  bool
}

// beginTurn resolves the conversation and write-aheads the request row.
// Mirrors the routing core's resolution rules: explicit ConversationID wins,
// else ExternalRef correlates, else a fresh conversation. Where capture is
// off the zero value is returned with capturing false — convID and
// prefixHash stay empty, which correctly disqualifies every ProviderGuard
// observation derived from it (see ProviderGuard.Observe's doc comment).
func (c *Client) beginTurn(ctx context.Context, meta SendMeta, req *msg.Request, primary string) turnRef {
	if c.capture == nil {
		return turnRef{}
	}
	drivenBy := meta.DrivenBy
	if drivenBy == "" {
		drivenBy = store.DrivenByServer
	}
	convRef := capture.ConversationRef{
		ID: meta.ConversationID, OriginEntrypoint: meta.OriginEntrypoint, DrivenBy: string(drivenBy),
		OwnerUserID: meta.OwnerUserID, Persona: meta.Persona, Model: req.Model, Name: meta.Name, ExternalRef: meta.ExternalRef,
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
		return turnRef{}
	}
	source := meta.Source
	if source == "" {
		source = meta.OriginEntrypoint
	}
	prefixHash := nativePrefixHash(req)
	turnID, createdAt, iErr := c.capture.InsertTurnIntent(ctx, capture.TurnIntent{
		ConversationID: convID, Ordinal: meta.Ordinal, Model: req.Model,
		Source: source, AuthorUserID: meta.AuthorUserID, AuthorKind: meta.AuthorKind,
		PrefixHash: prefixHash, Protocol: string(store.ProtocolRafiki),
		// The request body is not stored: the head names the rows it sent and
		// the trim range names what it left out.
		RequestHeadID: meta.RequestHeadID, TrimFrom: meta.TrimFrom, TrimTo: meta.TrimTo,
	})
	if iErr != nil {
		c.logger.Warn("capture: insert-intent failed (capturing disabled for this turn)", "error", iErr)
		return turnRef{}
	}
	// Record prefix_content (on-change) + cache_breakpoints for parity with the
	// proxy path — the message rows are written separately by Conversation, so
	// only the turn's prefix metadata is stored here (StoreTurnPrefix, not the
	// message-writing DecomposeRequest). The body is the envelope plus an empty
	// messages array, so the shared splitter sees the same shape it strips for a
	// real request; the breakpoints are the codec's placement, not re-derived
	// from markers. Best-effort: never fails the turn.
	if body, mErr := json.Marshal(struct {
		prefixEnvelope
		Messages []msg.Message `json:"messages"`
	}{prefixEnvelope: nativePrefixEnvelope(req), Messages: []msg.Message{}}); mErr != nil {
		c.logger.Warn("capture: marshal turn prefix failed", "error", mErr)
	} else if err := c.capture.StoreTurnPrefixBreakpoints(ctx, convID, turnID, createdAt, body, c.breakpointMessages(req, primary), prefixHash); err != nil {
		c.logger.Warn("capture: store turn prefix failed", "error", err)
	}
	return turnRef{turnID: turnID, createdAt: createdAt, convID: convID, prefixHash: prefixHash, capturing: true}
}

// breakpointMessages is the message indices the Anthropic codec places cache
// markers on, or nil for a provider that does not speak the Messages API. The
// capture row records where the markers land, so an operator can read the
// cache layout without re-encoding the request.
func (c *Client) breakpointMessages(req *msg.Request, primary string) []int {
	p, ok := c.set.Get(primary)
	if !ok || (p.Kind != providers.KindAnthropic && p.Kind != providers.KindAnthropicOpenRouter) {
		return nil
	}
	positions := msganthropic.Breakpoints(req)
	out := make([]int, 0, len(positions))
	for _, pos := range positions {
		if len(out) == 0 || out[len(out)-1] != pos.Message {
			out = append(out, pos.Message)
		}
	}
	return out
}

// completeTurn records the response; on a completion/marshal failure the turn
// is failed rather than stranded pending (mirrors the routing core).
func (c *Client) completeTurn(ctx context.Context, turnID string, createdAt time.Time, resp *msg.Response, upstream string, latencyMS int) {
	var cErr error
	reason := "complete-turn failed: "
	respJSON, mErr := msganthropic.ResponseJSON(resp)
	if mErr != nil {
		cErr, reason = mErr, "capture serialization failed: "
		c.logger.Warn("capture: marshal response failed", "error", mErr)
	} else if cErr = c.capture.CompleteTurn(ctx, capture.TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Model: resp.Model, Response: respJSON,
		StopReason: resp.StopReason.String(), Upstream: upstream,
		// OpenRouter's non-standard top-level "provider" field names the actual
		// backend that served the turn ("Together", "Parasail", …); "" for a
		// native Anthropic response, which has no such field.
		ServedProvider:      resp.Provider,
		InputTokens:         msg.Val(resp.Usage.Input),
		OutputTokens:        msg.Val(resp.Usage.Output),
		CacheReadTokens:     msg.Val(resp.Usage.CacheRead),
		CacheCreationTokens: msg.Val(resp.Usage.CacheWrite),
		LatencyMS:           latencyMS,
	}); cErr != nil {
		c.logger.Warn("capture: complete-turn write failed", "error", cErr)
	}
	if cErr != nil {
		if ferr := c.capture.FailTurn(ctx, turnID, createdAt, reason+cErr.Error()); ferr != nil {
			c.logger.Warn("capture: fail-turn fallback write failed", "error", ferr)
		}
	}
}

// recordRawTrace writes a debug raw_http_request row for one LLM API call.
// Best-effort: the insert itself runs in a background goroutine so a slow or
// unreachable database cannot add latency to the hot path (context.WithoutCancel
// only detaches the insert from the caller's cancellation, which is not enough
// on its own — the pgx call underneath is synchronous). The goroutine's context
// is still derived from ctx via WithoutCancel so the insert survives the
// request being cancelled or completing. Nothing joins the goroutine directly,
// but it is not a leak: it runs its single Exec against the same *pgxpool.Pool
// the daemon closes at shutdown, and pgxpool.Pool.Close blocks until every
// acquired connection is released, so daemon shutdown (see closePoolBounded in
// cmd/rafikid/main.go) already drains any insert still in flight, bounded by
// that same timeout. Errors are logged, never surfaced. No-op when c.rawTrace
// is nil.
//
// hdrs carries what headerCaptureTransport saw on the wire: the real request
// and response headers, the request path, and the request body DECOMPRESSED to
// the dialect's actual JSON (or a diagnostic when it could not be). Its fields
// stay nil when no attempt ever reached the transport (e.g. a resolution error
// before any sender was called).
func (c *Client) recordRawTrace(ctx context.Context, meta SendMeta, turnID string, req *msg.Request, resp *msg.Response, status int, upstream string, latency int, err error, hdrs *rawTraceHeaders) {
	if c.rawTrace == nil {
		return
	}
	if hdrs == nil {
		hdrs = &rawTraceHeaders{}
	}
	var respJSON json.RawMessage
	if resp != nil {
		b, mErr := msganthropic.ResponseJSON(resp)
		if mErr != nil {
			c.logger.Warn("raw trace: marshal response failed", "error", mErr)
		} else {
			respJSON = b
		}
	}

	var respStatus *int
	if status > 0 {
		respStatus = &status
	}
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	// A capture failure is a diagnostic, never an error of the send: it rides
	// the recorded Error field beside whatever the call itself returned.
	if hdrs.captureErr != "" {
		if errStr == "" {
			errStr = hdrs.captureErr
		} else {
			errStr += "; " + hdrs.captureErr
		}
	}

	r := rawtrace.RawHTTPRequest{
		Source:     "fundi",
		Model:      req.Model,
		Upstream:   upstream,
		ReqMethod:  "POST",
		ReqPath:    hdrs.reqPath,
		ReqHeaders: redactedHeaderJSON(hdrs.req, "Authorization", "X-Api-Key"),
		// The real wire body, decompressed. The headers above describe the
		// TRANSPORT (their Content-Encoding/Content-Length are the compressed
		// ones); this field is the JSON the provider actually parsed.
		ReqBody:     hdrs.reqBody,
		RespStatus:  respStatus,
		RespHeaders: redactedHeaderJSON(hdrs.resp),
		RespBody:    respJSON,
		LatencyMS:   latency,
		Error:       errStr,
	}
	if meta.ConversationID != "" {
		r.ConversationID = &meta.ConversationID
	}
	if turnID != "" {
		r.TurnID = &turnID
	}

	// detachedCtx, not ctx: the insert must survive the caller returning (or
	// being cancelled), and everything captured below (r, c.rawTrace, c.logger)
	// is either already-marshaled JSON or safe for concurrent use.
	detachedCtx := context.WithoutCancel(ctx)
	go func() {
		capCtx, cancel := context.WithTimeout(detachedCtx, 5*time.Second)
		defer cancel()
		_ = c.rawTrace.Insert(capCtx, r)
	}()
}

// redactedHeaderJSON marshals h as a flat map, replacing the value of every
// header named in redact (case-insensitively) with the literal string
// "<redacted>" — the header is still visible (so a captured row shows a
// credential was sent at all), just not its value. Nil input (no attempt
// reached the transport) maps to SQL NULL, matching nilJSON's convention.
func redactedHeaderJSON(h http.Header, redact ...string) json.RawMessage {
	if len(h) == 0 {
		return nil
	}
	strip := make(map[string]bool, len(redact))
	for _, k := range redact {
		strip[http.CanonicalHeaderKey(k)] = true
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if strip[http.CanonicalHeaderKey(k)] {
			out[k] = "<redacted>"
			continue
		}
		out[k] = strings.Join(vs, ", ")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}
