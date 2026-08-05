// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
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
	logger   *slog.Logger
	tracer   trace.Tracer

	breakerWindow time.Duration
	defaultModel  string
	modelGate     *ModelGate
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
func WithLogger(l *slog.Logger) ClientOption {
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
	return func(c *Client) { c.tracer = tp.Tracer("go.graveland.dev/rafiki/pkg/llm") }
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
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if c.tracer == nil {
		c.tracer = noop.NewTracerProvider().Tracer("go.graveland.dev/rafiki/pkg/llm")
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
	c.modelGate = NewModelGate(10*time.Second, 300*time.Second)
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
	Name             string
	ExternalRef      string
	Ordinal          int
	Source           string
	Author           string
	AuthorKind       string
	RateLimit        RateLimitPolicy

	Primary  Upstream   // empty = UpstreamAnthropic
	Fallback []Upstream // tried in order on retryable primary failure
}

// prepareSend is the ONE place that decides how a request is routed. Every
// send path (SendParams, sendStreaming) calls it first and MUST NOT
// re-derive any of this itself — a second copy would silently drift (e.g.
// streaming requests reaching a different upstream than non-streaming ones
// for the same model id). It:
//
//  1. Strips a native "anthropic/<x>" prefix so it isn't caught by the slash
//     rule below and so the native API receives a bare id.
//  2. Forces primary/fallbacks to (UpstreamOpenRouter, nil) for any
//     remaining slash id — an OpenRouter-native id must not default to
//     upstream anthropic.
//  3. Applies OpenRouter provider preferences when primary is OpenRouter,
//     BEFORE capture so the recorded request matches the wire.
//  4. Opens the "llm.send" tracing span (model/primary attributes) and
//     records current breaker state.
//
// Returns the span-carrying ctx, the span (caller must End it), the
// resolved primary/fallbacks, and params with any routing-driven mutation
// (prefix strip, provider prefs) applied.
func (c *Client) prepareSend(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams) (context.Context, trace.Span, Upstream, []Upstream, anthropic.MessageNewParams) {
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
	if b := c.breakers[primary]; b != nil {
		span.SetAttributes(attribute.Bool("rafiki.breaker.open", b.Open()))
	}
	return ctx, span, primary, fallbacks, params
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

// SendParams write-aheads the turn, routes the call through the primary's
// breaker with fallback, and completes/fails the turn. Capture is
// best-effort: a broken store degrades to pass-through, never blocks the
// call. Every invocation inserts its own turn row and always resolves it.
func (c *Client) SendParams(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	ctx, span, primary, fallbacks, params := c.prepareSend(ctx, meta, params)
	defer span.End()

	if err := c.modelGate.beforeSend(ctx, string(params.Model)); err != nil {
		return nil, err
	}

	turnID, turnCreatedAt, capturing := c.beginTurn(ctx, meta, params)

	start := time.Now()
	resp, servedBy, err := c.callModel(ctx, span, primary, fallbacks, params)
	latency := int(time.Since(start).Milliseconds())
	span.SetAttributes(attribute.String("rafiki.upstream", string(servedBy)))

	if err != nil {
		span.RecordError(err)
		c.failTurn(ctx, capturing, turnID, turnCreatedAt, err)
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

// sendStreaming issues params through the streaming path when the resolved
// primary sender implements StreamingSender, invoking handler once per event
// AS IT ARRIVES and accumulating the events into the returned message via
// anthropic.Message.Accumulate (the same accumulation pattern the SDK docs
// use for a caller driving stream.Next() itself).
//
// attempted reports whether the CALLER must fall back to SendParams itself:
//   - false means the caller must call SendParams — either no request was
//     sent at all here (the resolved sender doesn't implement
//     StreamingSender: no turn row, no side effect), or a request WAS sent
//     and failed, but delivered is also false (see below), so SendParams
//     retrying the whole send — including its full breaker/fallback chain
//     — is safe.
//   - true means the result here is final: either it succeeded, or it
//     failed after already delivering at least one event, in which case a
//     retry is never safe and the caller must surface err directly.
//
// delivered reports whether AT LEAST ONE event reached handler during this
// call. This is the structural guard sendWithTrim's trim-retry loop (and,
// transitively, sendAttempt's own-package caller) relies on: once delivered
// is true, retrying — here or in SendParams — is never safe, because a
// retry opens a brand new, independent request and the caller has no way to
// know the previous one already delivered partial output to handler. This
// holds regardless of what the resulting error turns out to be; nothing
// here or in sendWithTrim assumes a particular failure shape delivers
// before content.
//
// Failover: unlike an earlier version of this method, sendStreaming DOES
// engage even when a fallback chain and an active breaker are configured —
// it no longer bails out of streaming entirely just because both are set.
// A concrete consumer (fundi) enables Fallback(UpstreamOpenRouter) and
// WithBreaker together whenever an OpenRouter key is configured, which is
// the common case, not an edge case; bailing out of streaming there made
// WithStreamHandler silently a no-op for exactly the deployment this was
// built for.
//
// Breaker gate AND result recording both genuinely apply here, via the same
// primaryGate/recordPrimaryResult helpers callModel itself uses — this used
// to only be true of the gate (an earlier version of this comment claimed
// the breaker's semantics were "mirrored" while only the len(fallbacks) != 0
// && c.breakers[primary] != nil shape was reused, and the actual UsePrimary
// decision plus RecordResult bookkeeping were skipped entirely, so an open
// breaker never stopped a streaming send from reaching the dead primary and
// a streamed failure/success never updated breaker state). Concretely:
//   - Before issuing anything, primaryGate(primary, fallbacks, now) is
//     consulted. When it says NOT to use primary (breaker open, no probe
//     slot available), sendStreaming issues NOTHING — no request, no turn
//     row — and reports attempted=false so the caller's SendParams call
//     runs the complete breaker-gated fallback chain non-streamed, exactly
//     the handoff shape already used when the resolved sender lacks
//     streaming capability at all.
//   - When primaryGate does say to use primary, the returned breaker (nil
//     when bypassed — no fallback configured, or breakers disabled) is used
//     to recordPrimaryResult once the attempt resolves: success closes it,
//     a retryable failure trips/extends it, a non-retryable failure is left
//     alone — the identical rule callModel applies to its own sender.New()
//     call. EXCEPTION: recording is skipped exactly on the two pre-delivery
//     hand-off paths below (breaker != nil, nothing delivered yet) — the
//     caller's own follow-up callModel attempt is what records there, and
//     it runs moments later. Recording in both places would double-record
//     one logical failure and pre-emptively trip the breaker before that
//     retry ever gets a chance to run — this is not an oversight, it is
//     enforced per-branch below and is exactly the failure mode the
//     TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure and
//     TestSend_FailsOverWhenStreamDiesAfterMessageStartButBeforeContent
//     regression tests exist to catch if this "simplifies" back to an
//     unconditional call.
//   - breaker (the second primaryGate return, non-nil only when a fallback
//     chain AND an active breaker both genuinely apply) is ALSO still used,
//     as before, to decide what a PRE-delivery failure (delivered still
//     false) should report:
//   - breaker != nil: nothing has reached handler yet, so nothing can be
//     double-delivered by a retry. sendStreaming reports attempted=false;
//     the caller's subsequent SendParams call re-tries primary (subject to
//     the SAME breaker gate — see above) and, on a retryable failure, runs
//     the complete breaker-gated fallback chain non-streamed.
//   - breaker == nil (no fallback configured, or no breaker — the same
//     shape callModel itself would call directly with no failover to
//     offer): bouncing to SendParams would only re-issue to the identical
//     primary sender, wasting a request. sendStreaming instead reports
//     attempted=true so the caller's own retry logic (sendWithTrim's
//     trim-retry loop) can re-attempt via a fresh streaming call, exactly
//     as it always has.
//
// A MID-stream failure (delivered already true) is never safe to hand off
// regardless of breaker — see above — so sendStreaming always reports
// attempted=true there and the error surfaces directly, with no failover.
//
// Turn bookkeeping on a breaker-eligible pre-delivery handoff: this method still
// calls beginTurn before issuing the request and failTurn on the resulting
// error, so the streaming attempt's own turn row is written and correctly
// marked failed, even though attempted=false. The caller's subsequent
// SendParams call then begins and resolves ITS OWN, separate turn row for
// its own (possibly multi-upstream, via callModel) request. This is not an
// orphan or a duplicate: it is the same one-row-per-invocation convention
// sendWithTrim's own trim-retry loop already relies on (each retry attempt
// is its own SendParams call and so its own turn row) — two real requests
// were made here (one streamed and rejected, one non-streamed and
// resolved), so two rows accurately reflect that.
func (c *Client) sendStreaming(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams, handler StreamHandler) (*anthropic.Message, bool, bool, error) {
	rlp := meta.RateLimit.effective()
	model := string(params.Model)

	for attempt := 0; attempt <= rlp.MaxRetries; attempt++ {
		msg, attempted, delivered, err := c.sendStreamingAttempt(ctx, meta, params, handler)
		if err == nil || !attempted {
			return msg, attempted, delivered, err
		}
		// A streaming error with delivered==true is never retryable.
		if delivered {
			return msg, attempted, delivered, err
		}
		isRL, retryAfter := isRateLimit(err)
		if !isRL || attempt >= rlp.MaxRetries {
			return msg, attempted, delivered, err
		}
		c.modelGate.record429(model, retryAfter)
		c.logger.Warn("rate limit hit; retrying after backoff",
			"model", model, "attempt", attempt+1, "max", rlp.MaxRetries)
	}
	return nil, true, false, fmt.Errorf("llm: rate limit retries exhausted for model %s", model)
}

func (c *Client) sendStreamingAttempt(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams, handler StreamHandler) (msg *anthropic.Message, attempted bool, delivered bool, err error) {
	ctx, span, primary, fallbacks, params := c.prepareSend(ctx, meta, params)
	defer span.End()

	if err := c.modelGate.beforeSend(ctx, string(params.Model)); err != nil {
		return nil, false, false, err
	}

	sender := c.senders[primary]
	streamer, canStream := sender.(StreamingSender)
	if !canStream {
		return nil, false, false, nil
	}

	now := time.Now()
	usePrimary, breaker := c.primaryGate(primary, fallbacks, now)
	if !usePrimary {
		// The breaker is open and holds no probe slot right now: the primary
		// is known-bad. Issue nothing at all — no request, no turn row, the
		// same shape as the capability-bypass case above — so the caller's
		// SendParams call runs the complete breaker-gated fallback chain
		// non-streamed instead of burning a full timeout on a dead primary.
		c.logger.Warn("breaker open; stepping aside from streaming primary", "primary", string(primary))
		return nil, false, false, nil
	}

	turnID, turnCreatedAt, capturing := c.beginTurn(ctx, meta, params)
	start := time.Now()

	stream, serr := streamer.NewStreaming(ctx, params)
	// The defer is registered BEFORE the error checks below, and guarded on
	// stream != nil rather than serr == nil: the SDK's own sdkSender.NewStreaming
	// never returns a non-nil error (see its doc), but StreamingSender's
	// contract does not forbid a third-party implementation from returning
	// (non-nil stream, non-nil err). Registering the defer only after those
	// checks would leak that stream's decoder and response body on exactly
	// that path — this is what TestSendStreaming_ClosesStreamEvenWhenNewStreamingReturnsBothStreamAndError
	// pins down.
	if stream != nil {
		defer func() {
			if cerr := stream.Close(); cerr != nil {
				c.logger.Warn("llm: close stream failed", "error", cerr)
			}
		}()
	}
	if serr != nil {
		span.RecordError(serr)
		c.failTurn(ctx, capturing, turnID, turnCreatedAt, serr)
		if breaker != nil {
			// Nothing delivered yet and a real fallback exists: hand off
			// WITHOUT recording here. SendParams's callModel is about to make
			// its OWN authoritative attempt against primary and will record
			// THAT result; recording this pre-delivery failure too would
			// pre-emptively trip the breaker and make callModel skip the very
			// retry this handoff exists to give it.
			return nil, false, false, serr
		}
		// No fallback to offer: breaker is nil here (primaryGate's bypass
		// case), so there's nothing to record — report attempted=true so
		// sendWithTrim's own retry loop (not SendParams) re-attempts via
		// streaming again, instead of wastefully re-issuing to the same
		// primary sender.
		return nil, true, false, serr
	}

	acc := anthropic.Message{}
	for stream.Next() {
		ev := stream.Current()
		// Only content-bearing events count as delivered. message_start
		// carries no text, so treating it as delivery would make the failover
		// branch below unreachable for the common "connected, then died before
		// any output" failure — which the non-streaming path recovers from.
		if eventDeliversContent(ev) {
			delivered = true
		}
		handler(ev)
		if aerr := acc.Accumulate(ev); aerr != nil {
			wrapped := fmt.Errorf("llm: accumulate stream event: %w", aerr)
			span.RecordError(wrapped)
			c.failTurn(ctx, capturing, turnID, turnCreatedAt, wrapped)
			recordPrimaryResult(breaker, now, wrapped)
			return nil, true, delivered, wrapped
		}
		backfillDeltaUsage(&acc, ev)
	}
	if serr := stream.Err(); serr != nil {
		span.RecordError(serr)
		c.failTurn(ctx, capturing, turnID, turnCreatedAt, serr)
		if !delivered && breaker != nil {
			// Hand off WITHOUT recording, for the same reason as the
			// NewStreaming-error branch above: callModel's own imminent
			// primary retry is the authoritative attempt and will record its
			// own result. Recording this one too would trip the breaker
			// before that retry runs at all.
			return nil, false, false, serr
		}
		// Either something was already delivered (no handoff is possible
		// regardless of breaker state) or breaker is nil (bypass, no handoff
		// exists to defer to): this IS the final result, so record it.
		recordPrimaryResult(breaker, now, serr)
		return nil, true, delivered, serr
	}

	recordPrimaryResult(breaker, now, nil)
	c.modelGate.recordSuccess(string(params.Model))
	latency := int(time.Since(start).Milliseconds())
	span.SetAttributes(
		attribute.String("rafiki.upstream", string(primary)),
		attribute.Int64("rafiki.tokens.input", acc.Usage.InputTokens),
		attribute.Int64("rafiki.tokens.output", acc.Usage.OutputTokens),
		attribute.Int64("rafiki.tokens.cache_read", acc.Usage.CacheReadInputTokens),
		attribute.Int64("rafiki.tokens.cache_creation", acc.Usage.CacheCreationInputTokens),
	)
	if capturing {
		c.completeTurn(ctx, turnID, turnCreatedAt, &acc, primary, latency)
	}
	return &acc, true, delivered, nil
}

// eventDeliversContent reports whether ev is one of the content-bearing
// stream event variants — content_block_start/delta/stop — as opposed to
// message-envelope bookkeeping (message_start/delta/stop). A "ping"
// keep-alive never reaches here in the first place: the SDK's
// ssestream.Stream.Next() filters it out before Current() ever surfaces it.
//
// Uses the SDK's typed AsAny() discriminator (a switch on the real event
// struct type) rather than string-matching ev.Type, so this can't drift from
// the SDK's own variant set. AsAny() only returns one of the six known
// MessageStreamEventUnion variants for this SDK version (v1.37.0); the
// default case treats anything else (e.g. a variant added by a future SDK
// bump we haven't taught this switch about yet) as content-bearing —
// erring toward the ORIGINAL bug's safe direction (over-counting as
// delivered, which only narrows the failover window) rather than the
// unsafe one (under-counting, which risks double-delivery on retry).
func eventDeliversContent(ev anthropic.MessageStreamEventUnion) bool {
	switch ev.AsAny().(type) {
	case anthropic.ContentBlockStartEvent, anthropic.ContentBlockDeltaEvent, anthropic.ContentBlockStopEvent:
		return true
	case anthropic.MessageStartEvent, anthropic.MessageDeltaEvent, anthropic.MessageStopEvent:
		return false
	default:
		return true
	}
}

// backfillDeltaUsage corrects a gap in the SDK's own Message.Accumulate:
// message_delta only ever merges OutputTokens into the accumulated message,
// because for real Anthropic the input/cache token counts are fixed and
// reported once, correctly, in message_start — message_delta's own copies of
// those fields come back zero there, which is exactly why Accumulate ignores
// them.
//
// OpenRouter's Anthropic-compatible face does the reverse for at least two
// non-Anthropic backends (deepseek, moonshotai): message_start reports all
// prompt-side usage as zero, and the real cumulative input/cache token
// counts only appear in the final message_delta — verified directly against
// both APIs (a padded-prompt request against api.anthropic.com's real
// streaming endpoint keeps input_tokens fixed from message_start onward; the
// identical request against openrouter.ai/api's endpoint for
// deepseek/deepseek-v4-pro reports input_tokens:0 in message_start and the
// correct non-zero count only in message_delta). Accumulate silently drops
// that correction, so every OpenRouter-routed turn through this path
// persisted and displayed a real prompt with 0 input/cache tokens — visible
// as "0.0%" context usage in pi's attach TUI with no way to tell it apart
// from a genuinely token-free turn.
//
// Backfill whatever a delta actually populates (guarded on > 0, so a real
// Anthropic delta's zeroed fields never clobber message_start's correct
// values) — mirrors pkg/routing/sse.go's parseSSE, which already does this
// for the proxy's raw-bytes-based capture path; this is the equivalent for
// sendStreaming's typed-event path.
func backfillDeltaUsage(acc *anthropic.Message, ev anthropic.MessageStreamEventUnion) {
	delta, ok := ev.AsAny().(anthropic.MessageDeltaEvent)
	if !ok {
		return
	}
	if delta.Usage.InputTokens > 0 {
		acc.Usage.InputTokens = delta.Usage.InputTokens
	}
	if delta.Usage.CacheReadInputTokens > 0 {
		acc.Usage.CacheReadInputTokens = delta.Usage.CacheReadInputTokens
	}
	if delta.Usage.CacheCreationInputTokens > 0 {
		acc.Usage.CacheCreationInputTokens = delta.Usage.CacheCreationInputTokens
	}
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
func (c *Client) primaryGate(primary Upstream, fallbacks []Upstream, now time.Time) (usePrimary bool, breaker *routing.Breaker) {
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
func (c *Client) recordModelResult(params anthropic.MessageNewParams, err error) {
	if err == nil {
		c.modelGate.recordSuccess(string(params.Model))
		return
	}
	if isRL, retryAfter := isRateLimit(err); isRL {
		c.modelGate.record429(string(params.Model), retryAfter)
	}
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
	now := time.Now()
	usePrimary, breaker := c.primaryGate(primary, fallbacks, now)

	if breaker == nil {
		resp, err := sender.New(ctx, params)
		c.recordModelResult(params, err)
		return resp, primary, err
	}

	if usePrimary {
		resp, err := sender.New(ctx, params)
		recordPrimaryResult(breaker, now, err)
		c.recordModelResult(params, err)
		if err == nil {
			return resp, primary, nil
		}
		if !routing.FailoverWorthy(err) {
			return nil, primary, err // not failover-worthy: don't fail over or trip
		}
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
			c.modelGate.recordSuccess(string(params.Model))
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
		Owner: meta.Owner, Persona: meta.Persona, Model: string(params.Model), Name: meta.Name, ExternalRef: meta.ExternalRef,
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
