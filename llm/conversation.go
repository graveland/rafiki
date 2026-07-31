// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/routing"
	"go.graveland.dev/rafiki/store"
)

// Conversation is the core library object: DB-backed message history plus
// conversation-level defaults. Sends load history, assemble the request
// internally (system cache breakpoint, deterministic tools, trimming,
// prefix_hash) and persist both granularities (messages = state, turn =
// evidence).
//
// A Conversation handle is owned by ONE goroutine at a time: methods are not
// safe for concurrent use (concurrent sends would race on ordinals; the
// store's ordinal uniqueness turns such races into errors, not corruption).
type Conversation struct {
	client *Client
	ID     string
	cfg    convConfig

	// mem holds history when the client has no store: same loop semantics,
	// no capture and no Resume (both need the DB). CLI-mode hosts use this.
	mem []store.Message
}

type convConfig struct {
	// identity/creation
	externalRef string
	owner       string
	entrypoint  string
	persona     string

	// send defaults
	model          string // resolved to a concrete id at Conversation()
	primary        Upstream
	thinkingBudget int64
	fallback       []Upstream
	system         []anthropic.TextBlockParam
	temperature    *float64
	topK           *int64
	maxTokens      int64
	trim           TrimPolicy
	authorKind     string
	cache          *CachePolicy
}

type ConvOption func(*convConfig)

// ByExternalRef correlates (or creates) the conversation by an external key —
// e.g. a Slack thread ts — race-safe via the store's partial unique index.
func ByExternalRef(ref string) ConvOption {
	return func(c *convConfig) { c.externalRef = ref }
}

// NewConversation creates a fresh conversation owned by owner, originating
// from entrypoint (e.g. "diagnose", "coredump").
func NewConversation(owner, entrypoint string) ConvOption {
	return func(c *convConfig) { c.owner, c.entrypoint = owner, entrypoint }
}

// Entrypoint sets origin_entrypoint when combined with ByExternalRef.
func Entrypoint(e string) ConvOption { return func(c *convConfig) { c.entrypoint = e } }

// Persona records the persona used for this conversation.
func Persona(p string) ConvOption { return func(c *convConfig) { c.persona = p } }

// Model sets the conversation's model. Aliases ("<family>-latest", short
// model aliases like "kimi-k3") resolve to a concrete id once, at
// Conversation creation, and stay fixed for the conversation's life (prefix
// stability). A slash (OpenRouter-native) id routes every send directly to
// OpenRouter, with no failover.
func Model(m string) ConvOption { return func(c *convConfig) { c.model = m } }

// Fallback sets the upstream fallback chain consulted on retryable primary
// failures.
func Fallback(u ...Upstream) ConvOption { return func(c *convConfig) { c.fallback = u } }

// Primary selects the upstream used first for every send in this conversation
// (empty = UpstreamAnthropic, the default). The Fallback chain still applies.
func Primary(u Upstream) ConvOption { return func(c *convConfig) { c.primary = u } }

// ThinkingBudget enables extended thinking with the given token budget
// (0 = disabled, the default).
func ThinkingBudget(tokens int64) ConvOption {
	return func(c *convConfig) { c.thinkingBudget = tokens }
}

// Temperature sets the sampling temperature default.
func Temperature(t float64) ConvOption { return func(c *convConfig) { c.temperature = &t } }

// TopK sets the top-K sampling default.
func TopK(k int64) ConvOption { return func(c *convConfig) { c.topK = &k } }

// MaxTokens sets the default output cap (4096 when unset).
func MaxTokens(n int64) ConvOption { return func(c *convConfig) { c.maxTokens = n } }

// System sets the system prompt blocks. Prompt-cache breakpoints are applied
// internally at request assembly per the conversation's CachePolicy — callers
// never set cache_control themselves. System is immutable for the
// conversation's life.
func System(blocks []anthropic.TextBlockParam) ConvOption {
	return func(c *convConfig) { c.system = blocks }
}

// SystemText is the single-block convenience form of System.
func SystemText(s string) ConvOption {
	return System([]anthropic.TextBlockParam{{Text: s}})
}

// WithTrimPolicy sets the prompt-too-large trim policy (default: keep the
// first message + the most recent within a halving 300KB byte budget).
func WithTrimPolicy(p TrimPolicy) ConvOption { return func(c *convConfig) { c.trim = p } }

// AuthorKind stamps turn provenance ('human' | 'agent' | 'system').
func AuthorKind(k string) ConvOption { return func(c *convConfig) { c.authorKind = k } }

// CachePolicy controls where the conversation places prompt-cache
// breakpoints when assembling each request. SystemTTL caches the static
// prefix (tools render before system, so one breakpoint on the last system
// block covers both). MessagesTTL enables moving breakpoints over the
// message history — the standard multi-turn pattern: the last content block
// of the request is marked each send, so turn N reads the whole history from
// cache and pays only the delta. Breakpoints (clamped to [1,3]) is how many
// moving markers to place; the extras trail every ~15 blocks to stay inside
// the API's 20-block cache lookback on tool-heavy turns. At most 4 total
// breakpoints are used (1 system + up to 3 moving), the API maximum.
type CachePolicy struct {
	SystemTTL   CacheTTL
	MessagesTTL CacheTTL
	Breakpoints int
}

// DefaultCachePolicy caches everything at the 5-minute TTL with two moving
// breakpoints: the cheapest policy that keeps an active conversation fully
// cached. Callers with cross-conversation prefix reuse inside an hour (batch
// analyzers, busy alert loops) should raise SystemTTL to Cache1h.
func DefaultCachePolicy() CachePolicy {
	return CachePolicy{SystemTTL: Cache5m, MessagesTTL: Cache5m, Breakpoints: 2}
}

// WithCache overrides the conversation's prompt-cache policy (default:
// DefaultCachePolicy). Each application takes its own copy: Conversation()
// clamps Breakpoints in place, and sharing &p across conversations built
// from one reused option would leak that clamp between them.
func WithCache(p CachePolicy) ConvOption {
	return func(c *convConfig) {
		q := p
		c.cache = &q
	}
}

// Conversation loads or creates a conversation. With WithStore it is
// DB-backed (captured, resumable); without, it degrades to an in-memory
// history with identical loop semantics.
func (c *Client) Conversation(ctx context.Context, opts ...ConvOption) (*Conversation, error) {
	cfg := convConfig{maxTokens: 4096, trim: defaultTrimPolicy{}, authorKind: "agent"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.cache == nil {
		d := DefaultCachePolicy()
		cfg.cache = &d
	}
	if cfg.cache.Breakpoints < 1 {
		cfg.cache.Breakpoints = 1
	} else if cfg.cache.Breakpoints > 3 {
		cfg.cache.Breakpoints = 3
	}
	if cfg.entrypoint == "" {
		return nil, errors.New("llm: an entrypoint is required (NewConversation or Entrypoint)")
	}

	// Resolve the model once: aliases pin to a concrete id for the whole
	// conversation so the cached prefix can't drift with catalog refreshes.
	resolved, err := routing.ResolveModel(c.catalog, c.defaultModel, cfg.model)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	cfg.model = resolved

	if c.capture == nil {
		// Store-less (in-memory) conversation: full loop semantics, no
		// capture and no Resume. ID is local-only.
		return &Conversation{client: c, ID: "mem-" + randomID(), cfg: cfg}, nil
	}
	ref := routing.ConversationRef{
		OriginEntrypoint: cfg.entrypoint, DrivenBy: string(store.DrivenByServer),
		Owner: cfg.owner, Persona: cfg.persona, Model: cfg.model, ExternalRef: cfg.externalRef,
	}
	var convID string
	if cfg.externalRef != "" {
		convID, err = c.capture.EnsureConversationByExternalRef(ctx, ref)
	} else {
		convID, err = c.capture.EnsureConversation(ctx, ref)
	}
	if err != nil {
		return nil, fmt.Errorf("llm: ensure conversation: %w", err)
	}
	return &Conversation{client: c, ID: convID, cfg: cfg}, nil
}

// UserText builds a single-text-block user message payload for Send.
func UserText(s string) []anthropic.ContentBlockParamUnion {
	return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(s)}
}

type sendConfig struct {
	maxTokens     int64
	tools         []anthropic.ToolUnionParam
	source        string
	author        string
	toolChoice    string
	streamHandler StreamHandler
}

type SendOption func(*sendConfig)

// StreamHandler receives each streaming event as it arrives.
type StreamHandler func(ev anthropic.MessageStreamEventUnion)

// WithStreamHandler streams the response, invoking h once per event as it
// arrives, and returns the accumulated message exactly as an unstreamed call
// would.
//
// h is ignored (never invoked, and the call transparently falls back to the
// non-streamed path) in two cases: when the resolved sender does not
// implement StreamingSender, or when the primary's circuit breaker is open.
// In both cases the send behaves exactly as if WithStreamHandler had not
// been passed at all — a caller never has to branch on sender capability,
// but it also means a caller relying on h firing must not assume it always
// will.
//
// h is called synchronously on the calling goroutine: never concurrently
// with itself, and never after Send/Continue returns. No locking is
// required in h.
//
// IMPORTANT: h may be invoked one or more times and the call may STILL
// return an error — a stream can die after delivering content (see
// sendStreaming's doc for the delivered/attempted bookkeeping this drives).
// A consumer rendering h's output must be prepared for a torn, incomplete
// message and must not treat "h fired" as "the turn succeeded". Completion
// is signalled only by Send/Continue returning a nil error.
func WithStreamHandler(h StreamHandler) SendOption {
	return func(s *sendConfig) { s.streamHandler = h }
}

// WithMaxTokens overrides the output cap for this send only.
func WithMaxTokens(n int64) SendOption { return func(s *sendConfig) { s.maxTokens = n } }

// WithTools attaches tool definitions for this send. Definitions MUST be in
// a deterministic order across a conversation's sends (any stable order —
// e.g. sc's sorted-local + sorted-upstream concatenation): a reordered tools
// array silently busts the Anthropic prompt-cache prefix. The library cannot
// verify cross-call stability; prefix_hash on captured turns is the drift
// detector.
func WithTools(tools []anthropic.ToolUnionParam) SendOption {
	return func(s *sendConfig) { s.tools = tools }
}

// WithSource overrides the per-turn inbound entrypoint.
func WithSource(src string) SendOption { return func(s *sendConfig) { s.source = src } }

// WithAuthor stamps the per-turn author identity.
func WithAuthor(a string) SendOption { return func(s *sendConfig) { s.author = a } }

// WithToolChoice forces the named tool for this send only.
func WithToolChoice(name string) SendOption {
	return func(s *sendConfig) { s.toolChoice = name }
}

// Send appends userContent to the conversation and gets the assistant's
// reply: load history → persist the user message (write-ahead) → assemble →
// call (breaker/fallback/capture, prompt-too-large trim-retry) → persist the
// assistant message + turn. Returns the assistant response.
func (conv *Conversation) Send(ctx context.Context, userContent []anthropic.ContentBlockParamUnion, opts ...SendOption) (*anthropic.Message, error) {
	if err := conv.AppendUser(ctx, userContent); err != nil {
		return nil, err
	}
	return conv.Continue(ctx, opts...)
}

// AppendUser persists user content as the next message row WITHOUT sending —
// the write-ahead primitive the agent loop uses to persist tool results (one
// row per result) before continuing. Consecutive user rows merge into a
// single wire message at request assembly.
func (conv *Conversation) AppendUser(ctx context.Context, content []anthropic.ContentBlockParamUnion) error {
	history, err := conv.loadHistory(ctx)
	if err != nil {
		return fmt.Errorf("llm: %w", err)
	}
	userParam := anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: content}
	if err := conv.appendMessage(ctx, nextOrdinal(history), userParam, nil); err != nil {
		return fmt.Errorf("llm: %w", err)
	}
	return nil
}

// Continue sends the conversation AS STORED — no new user message — and
// persists the assistant reply. This is the loop-turn / crash-recovery
// primitive: a Resume with a pending turn just Continues, re-issuing the
// interrupted call from persisted state.
func (conv *Conversation) Continue(ctx context.Context, opts ...SendOption) (*anthropic.Message, error) {
	scfg := sendConfig{maxTokens: conv.cfg.maxTokens}
	for _, opt := range opts {
		opt(&scfg)
	}

	ctx, span := conv.client.tracer.Start(ctx, "llm.conversation.send",
		trace.WithAttributes(attribute.String("rafiki.conversation", conv.ID)))
	defer span.End()

	history, err := conv.loadHistory(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	if len(history) == 0 {
		return nil, errors.New("llm: Continue on an empty conversation (nothing to send)")
	}

	resp, err := conv.sendWithTrim(ctx, span, nextOrdinal(history), mergeForRequest(history), scfg)
	if err != nil {
		return nil, err
	}

	assistant := resp.ToParam()
	if err := conv.appendMessage(ctx, nextOrdinal(history), assistant,
		&store.AssistantMeta{Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens,
			StopReason: string(resp.StopReason)}); err != nil {
		// The turn is already captured; a failed message append must be loud —
		// resume protocol depends on message rows.
		return nil, fmt.Errorf("llm: persist assistant message: %w", err)
	}
	return resp, nil
}

// History returns the stored messages (ordinal order) — the agent loop's
// orphan-analysis input.
func (conv *Conversation) History(ctx context.Context) ([]store.Message, error) {
	return conv.loadHistory(ctx)
}

// IncrementResumeAttempts bumps the per-conversation Resume counter.
func (conv *Conversation) IncrementResumeAttempts(ctx context.Context) (int, error) {
	if conv.client.pool == nil {
		return 0, errors.New("llm: Resume requires WithStore (in-memory conversations are not recoverable)")
	}
	return store.IncrementResumeAttempts(ctx, conv.client.pool, conv.ID)
}

// MarkFailed sets the conversation status to failed (Resume cap exceeded).
func (conv *Conversation) MarkFailed(ctx context.Context) error {
	if conv.client.pool == nil {
		return errors.New("llm: MarkFailed requires WithStore")
	}
	return store.SetConversationStatus(ctx, conv.client.pool, conv.ID, store.ConversationFailed)
}

// ResetResumeAttempts zeroes the resume counter after a successful recovery —
// the cap counts consecutive failures, not lifetime interruptions.
func (conv *Conversation) ResetResumeAttempts(ctx context.Context) error {
	if conv.client.pool == nil {
		return errors.New("llm: ResetResumeAttempts requires WithStore")
	}
	return store.ResetResumeAttempts(ctx, conv.client.pool, conv.ID)
}

// RetractTailUser removes the trailing user message at ordinal — write-ahead
// retraction for a first send the API rejected as too large, so the host can
// rebuild a smaller prompt and retry on the SAME conversation. Refuses
// anything that isn't the tail user row.
func (conv *Conversation) RetractTailUser(ctx context.Context, ordinal int) error {
	if conv.client.messages == nil {
		for i, m := range conv.mem {
			if m.Ordinal == ordinal && i == len(conv.mem)-1 && m.Param.Role == anthropic.MessageParamRoleUser {
				conv.mem = conv.mem[:i]
				return nil
			}
		}
		return fmt.Errorf("llm: ordinal %d is not the tail user row", ordinal)
	}
	return store.DeleteTailUserMessage(ctx, conv.client.pool, conv.ID, ordinal)
}

// loadHistory reads history through the storage seam (DB rows or in-memory).
func (conv *Conversation) loadHistory(ctx context.Context) ([]store.Message, error) {
	if conv.client.messages == nil {
		return conv.mem, nil
	}
	return conv.client.messages.Load(ctx, conv.ID)
}

// appendMessage writes through the storage seam, preserving the store's
// ordinal-idempotent divergence semantics in memory.
func (conv *Conversation) appendMessage(ctx context.Context, ordinal int, param anthropic.MessageParam, meta *store.AssistantMeta) error {
	if conv.client.messages == nil {
		for _, m := range conv.mem {
			if m.Ordinal == ordinal {
				a, _ := json.Marshal(m.Param.Content)
				b, _ := json.Marshal(param.Content)
				if string(a) != string(b) {
					return fmt.Errorf("llm: ordinal %d already holds different content (history diverged)", ordinal)
				}
				return nil
			}
		}
		msg := store.Message{Ordinal: ordinal, Param: param, ToolUseIDs: store.ToolUseIDsOf(param)}
		if meta != nil {
			msg.StopReason = meta.StopReason
		}
		conv.mem = append(conv.mem, msg)
		return nil
	}
	return conv.client.messages.Append(ctx, conv.ID, ordinal, param, meta)
}

// SeedHistory idempotently persists msgs at ordinals 0..n-1 — the bridge for
// hosts carrying client-side history (e.g. Slack thread continuations):
// already-persisted identical rows no-op, diverging content errors.
func (conv *Conversation) SeedHistory(ctx context.Context, msgs []Message) error {
	for i, m := range msgs {
		if err := conv.appendMessage(ctx, i, m, nil); err != nil {
			return err
		}
	}
	return nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// mergeForRequest converts stored rows to wire messages, merging CONSECUTIVE
// user rows into one user message (per-tool tool_result rows → the single
// user message the API requires after an assistant tool_use turn). Block
// order within the merge follows row ordinal order.
func mergeForRequest(history []store.Message) []Message {
	out := make([]Message, 0, len(history))
	for _, m := range history {
		if m.Param.Role == anthropic.MessageParamRoleUser && len(out) > 0 &&
			out[len(out)-1].Role == anthropic.MessageParamRoleUser {
			last := &out[len(out)-1]
			// Clone before appending: appending into a stored param's backing
			// array would mutate history the caller may still hold.
			last.Content = append(slices.Clone(last.Content), m.Param.Content...)
			continue
		}
		out = append(out, m.Param)
	}
	return out
}

// sendWithTrim issues the call, applying the trim policy on prompt-too-large
// (max 3 attempts). Trimming reshapes only the request message list — stored
// rows and the system/tools prefix are untouched, so the cached prefix
// survives every retry (prefix_hash equality across attempts is the
// regression test).
func (conv *Conversation) sendWithTrim(ctx context.Context, span trace.Span, ordinal int, reqMsgs []Message, scfg sendConfig) (*anthropic.Message, error) {
	meta := SendMeta{
		ConversationID:   conv.ID,
		OriginEntrypoint: conv.cfg.entrypoint,
		DrivenBy:         store.DrivenByServer,
		Owner:            conv.cfg.owner,
		Persona:          conv.cfg.persona,
		Ordinal:          ordinal, // history-derived: stable across handles and resumes
		Source:           scfg.source,
		Author:           scfg.author,
		AuthorKind:       conv.cfg.authorKind,
		Primary:          conv.cfg.primary,
		Fallback:         conv.cfg.fallback,
	}
	for attempt := 0; attempt < 3; attempt++ {
		params := conv.assemble(reqMsgs, scfg)
		resp, delivered, err := conv.sendAttempt(ctx, meta, params, scfg.streamHandler)
		if err == nil {
			return resp, nil
		}
		if delivered {
			// Structural trim-retry guard: at least one event already reached
			// scfg.streamHandler for THIS attempt. Retrying now would open a
			// second, independent stream for the same logical send with no way
			// for the caller to know the first was abandoned mid-flight — never
			// safe, regardless of what err turns out to be (isPromptTooLarge or
			// not). So this is checked before, and independent of, the
			// isPromptTooLarge check below.
			return nil, err
		}
		if !isPromptTooLarge(err) {
			return nil, err
		}
		trimmed, ok := conv.cfg.trim.Trim(reqMsgs, attempt)
		if !ok {
			return nil, err
		}
		span.AddEvent("trim", trace.WithAttributes(
			attribute.Int("rafiki.trim.attempt", attempt),
			attribute.Int("rafiki.trim.before", len(reqMsgs)),
			attribute.Int("rafiki.trim.after", len(trimmed)),
		))
		conv.client.logger.Warn("prompt too large; trimmed history for retry",
			"conversation", conv.ID, "attempt", attempt, "messages_before", len(reqMsgs), "messages_after", len(trimmed))
		reqMsgs = trimmed
	}
	return nil, errors.New("llm: prompt still too large after trim retries")
}

// sendAttempt issues ONE request attempt, streaming through handler when set
// and the resolved sender supports it, and transparently falling back to the
// plain (non-streaming) path otherwise — the caller (sendWithTrim) never
// branches on sender capability. delivered reports whether any event reached
// handler during this attempt; see sendStreaming's doc for why the
// trim-retry loop treats that as an absolute "never retry" signal.
func (conv *Conversation) sendAttempt(ctx context.Context, meta SendMeta, params anthropic.MessageNewParams, handler StreamHandler) (resp *anthropic.Message, delivered bool, err error) {
	if handler == nil {
		resp, err = conv.client.SendParams(ctx, meta, params)
		return resp, false, err
	}
	resp, attempted, delivered, err := conv.client.sendStreaming(ctx, meta, params, handler)
	if !attempted {
		resp, err = conv.client.SendParams(ctx, meta, params)
		return resp, false, err
	}
	return resp, delivered, err
}

// assemble builds the wire request and places prompt-cache breakpoints per
// the conversation's CachePolicy: one on the LAST system block (tools render
// before system in the Anthropic prefix, so it caches tool schemas + system
// prompt together) and up to three moving breakpoints over the message
// history (see CachePolicy).
func (conv *Conversation) assemble(reqMsgs []Message, scfg sendConfig) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(conv.cfg.model),
		MaxTokens: scfg.maxTokens,
		Messages:  reqMsgs,
	}
	policy := conv.cfg.cache
	if policy == nil { // direct-construction (tests) safety; Conversation() always sets it
		d := DefaultCachePolicy()
		policy = &d
	}
	if len(conv.cfg.system) > 0 {
		system := make([]anthropic.TextBlockParam, len(conv.cfg.system))
		copy(system, conv.cfg.system)
		if cc, on := cacheControlFor(policy.SystemTTL); on {
			system[len(system)-1].CacheControl = cc
		}
		params.System = system
	}
	params.Messages = withMessageBreakpoints(reqMsgs, policy)
	if len(scfg.tools) > 0 {
		params.Tools = scfg.tools
	}
	if scfg.toolChoice != "" {
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: scfg.toolChoice},
		}
	}
	if conv.cfg.temperature != nil {
		params.Temperature = anthropic.Float(*conv.cfg.temperature)
	}
	if conv.cfg.topK != nil {
		params.TopK = anthropic.Int(*conv.cfg.topK)
	}
	if conv.cfg.thinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(conv.cfg.thinkingBudget)
	}
	return params
}

// cacheControlFor maps a CacheTTL to its wire form. on=false for CacheOff
// (and the zero value, so an unset field never emits a breakpoint).
func cacheControlFor(ttl CacheTTL) (anthropic.CacheControlEphemeralParam, bool) {
	switch ttl {
	case Cache5m:
		return anthropic.NewCacheControlEphemeralParam(), true // TTL omitted = 5m default
	case Cache1h:
		cc := anthropic.NewCacheControlEphemeralParam()
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL1h
		return cc, true
	default:
		return anthropic.CacheControlEphemeralParam{}, false
	}
}

// lookbackStride spaces trailing moving breakpoints so each stays within the
// API's 20-block cache lookback of the previous entry on block-heavy turns.
const lookbackStride = 15

// withMessageBreakpoints returns the request's message list with up to
// p.Breakpoints moving cache markers, walking backward from the last
// cacheable block (thinking/redacted blocks can't carry cache_control and
// are skipped, but still count toward the lookback stride). Stale markers —
// possible on histories reloaded from capture, whose stored content is the
// verbatim prior request — are removed so the request never exceeds the
// API's 4-breakpoint limit. STRICTLY copy-on-write: the caller's messages
// (the conversation's shared history) are never mutated; only the affected
// blocks are cloned. This keeps previously-captured request params stable.
func withMessageBreakpoints(msgs []Message, p *CachePolicy) []Message {
	cc, on := cacheControlFor(p.MessagesTTL)

	type edit struct {
		i, j int
		cc   anthropic.CacheControlEphemeralParam
	}
	var edits []edit

	placed, walked := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		content := msgs[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			walked++
			ptr := content[j].GetCacheControl()
			if ptr == nil {
				continue
			}
			pick := on && placed < p.Breakpoints && (placed == 0 || walked >= lookbackStride)
			switch {
			case pick:
				edits = append(edits, edit{i, j, cc})
				placed++
				walked = 0
			case ptr.Type != "" || ptr.TTL != "":
				edits = append(edits, edit{i, j, anthropic.CacheControlEphemeralParam{}}) // clear stale marker
			}
		}
	}
	if len(edits) == 0 {
		return msgs
	}

	out := make([]Message, len(msgs))
	copy(out, msgs)
	copied := map[int]bool{}
	for _, e := range edits {
		if !copied[e.i] {
			nc := make([]anthropic.ContentBlockParamUnion, len(out[e.i].Content))
			copy(nc, out[e.i].Content)
			out[e.i].Content = nc
			copied[e.i] = true
		}
		out[e.i].Content[e.j] = blockWithCacheControl(out[e.i].Content[e.j], e.cc)
	}
	return out
}

// blockWithCacheControl returns a copy of the block with cc applied, leaving
// the original untouched. Covers every union member that carries
// cache_control (mirrors ContentBlockParamUnion.GetCacheControl); a
// non-cacheable block is returned unchanged.
func blockWithCacheControl(b anthropic.ContentBlockParamUnion, cc anthropic.CacheControlEphemeralParam) anthropic.ContentBlockParamUnion {
	switch {
	case b.OfText != nil:
		v := *b.OfText
		v.CacheControl = cc
		b.OfText = &v
	case b.OfImage != nil:
		v := *b.OfImage
		v.CacheControl = cc
		b.OfImage = &v
	case b.OfDocument != nil:
		v := *b.OfDocument
		v.CacheControl = cc
		b.OfDocument = &v
	case b.OfSearchResult != nil:
		v := *b.OfSearchResult
		v.CacheControl = cc
		b.OfSearchResult = &v
	case b.OfToolUse != nil:
		v := *b.OfToolUse
		v.CacheControl = cc
		b.OfToolUse = &v
	case b.OfToolResult != nil:
		v := *b.OfToolResult
		v.CacheControl = cc
		b.OfToolResult = &v
	case b.OfServerToolUse != nil:
		v := *b.OfServerToolUse
		v.CacheControl = cc
		b.OfServerToolUse = &v
	case b.OfWebSearchToolResult != nil:
		v := *b.OfWebSearchToolResult
		v.CacheControl = cc
		b.OfWebSearchToolResult = &v
	case b.OfWebFetchToolResult != nil:
		v := *b.OfWebFetchToolResult
		v.CacheControl = cc
		b.OfWebFetchToolResult = &v
	case b.OfCodeExecutionToolResult != nil:
		v := *b.OfCodeExecutionToolResult
		v.CacheControl = cc
		b.OfCodeExecutionToolResult = &v
	case b.OfBashCodeExecutionToolResult != nil:
		v := *b.OfBashCodeExecutionToolResult
		v.CacheControl = cc
		b.OfBashCodeExecutionToolResult = &v
	case b.OfTextEditorCodeExecutionToolResult != nil:
		v := *b.OfTextEditorCodeExecutionToolResult
		v.CacheControl = cc
		b.OfTextEditorCodeExecutionToolResult = &v
	case b.OfToolSearchToolResult != nil:
		v := *b.OfToolSearchToolResult
		v.CacheControl = cc
		b.OfToolSearchToolResult = &v
	case b.OfContainerUpload != nil:
		v := *b.OfContainerUpload
		v.CacheControl = cc
		b.OfContainerUpload = &v
	}
	return b
}

func nextOrdinal(history []store.Message) int {
	if len(history) == 0 {
		return 0
	}
	return history[len(history)-1].Ordinal + 1
}
