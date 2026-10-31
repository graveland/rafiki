// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/capture"
	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// CacheTTL is a prompt-cache breakpoint lifetime.
type CacheTTL = msg.CacheTTL

const (
	CacheOff = msg.CacheOff // no breakpoint
	Cache1h  = msg.Cache1h  // 2x write premium; pays off with cross-conversation reuse
	Cache5m  = msg.Cache5m  // 1.25x write premium; entry lives 5 minutes
)

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
//
// Placement itself is the Anthropic codec's (msganthropic.Breakpoints): a
// request carries a policy, never markers.
type CachePolicy = msg.CachePolicy

// DefaultCachePolicy caches everything at the 5-minute TTL with two moving
// breakpoints: the cheapest policy that keeps an active conversation fully
// cached. Callers with cross-conversation prefix reuse inside an hour (batch
// analyzers, busy alert loops) should raise SystemTTL to Cache1h.
func DefaultCachePolicy() CachePolicy {
	return CachePolicy{SystemTTL: Cache5m, MessagesTTL: Cache5m, Breakpoints: 2}
}

// Conversation is the core library object: DB-backed message history plus
// conversation-level defaults. Sends load history, assemble the request
// internally (system cache policy, deterministic tools, trimming, prefix_hash)
// and persist both granularities (messages = state, turn = evidence).
//
// A Conversation handle is owned by ONE goroutine at a time: methods are not
// safe for concurrent use (two concurrent sends would each move the head, and
// the capture and turn ordinals assume one writer).
type Conversation struct {
	client *Client
	ID     string
	cfg    convConfig

	// usedTokens is the total token count the last successful response
	// reported (input + cache read + cache creation + output); 0 means "not
	// known yet", which the compaction trigger reads as "estimate from bytes
	// instead".
	usedTokens int
	// onCompact observes compaction boundaries; nil means no observer.
	//
	// usedTokens and onCompact are UNSYNCHRONISED: a Conversation is owned by
	// one goroutine at a time, so a caller must never run two sends
	// concurrently.
	onCompact func(CompactionEvent)

	// compactSuppressBelow is the usedForCheck value below which proactive
	// compaction stays suppressed after a FAILED attempt. A persistently
	// failing summary (e.g. truncated at SummaryMaxTokens) would otherwise be
	// retried on every tool iteration — each retry a full-context call. After
	// a failure it is set to usedAtFailure + HeadroomBuffer/2, so the next
	// proactive attempt waits until the working set has grown by at least half
	// the headroom buffer. Cleared on a successful compaction. The reactive
	// overflow net is unaffected: it is armed regardless of this value.
	compactSuppressBelow int
}

type convConfig struct {
	// identity/creation
	externalRef string
	ownerUserID string
	entrypoint  string
	persona     string
	name        string

	// send defaults
	model          string // resolved to a concrete id at Conversation()
	primary        string
	thinkingBudget int64
	fallback       []string
	system         []msg.Text
	temperature    *float64
	topK           *int64
	maxTokens      int64
	trim           TrimPolicy
	authorKind     string
	rateLimit      RateLimitPolicy
	cache          *CachePolicy
	compaction     *CompactionPolicy
}

type ConvOption func(*convConfig)

// ByExternalRef correlates (or creates) the conversation by an external key —
// e.g. a Slack thread ts — race-safe via the store's partial unique index.
func ByExternalRef(ref string) ConvOption {
	return func(c *convConfig) { c.externalRef = ref }
}

// NewConversation creates a fresh conversation owned by ownerUserID — a
// conversations.users id, NOT a username — originating from entrypoint (e.g.
// "diagnose", "coredump"). Empty means unattributed, which is what a daemon-run
// conversation (the analyzer, a core-dump pass) should pass: it runs as the
// daemon, not as a person.
func NewConversation(ownerUserID, entrypoint string) ConvOption {
	return func(c *convConfig) { c.ownerUserID, c.entrypoint = ownerUserID, entrypoint }
}

// Entrypoint sets origin_entrypoint when combined with ByExternalRef.
func Entrypoint(e string) ConvOption { return func(c *convConfig) { c.entrypoint = e } }

// Persona records the persona used for this conversation.
func Persona(p string) ConvOption { return func(c *convConfig) { c.persona = p } }

// WithName sets a human-readable name on the conversation.
func WithName(n string) ConvOption { return func(c *convConfig) { c.name = n } }

// Model sets the conversation's model. A "<family>-latest" alias resolves to
// a concrete id once, at Conversation creation, and stays fixed for the
// conversation's life (prefix stability). A slash (OpenRouter-native) id routes every send directly to
// OpenRouter, with no failover.
func Model(m string) ConvOption { return func(c *convConfig) { c.model = m } }

// Fallback sets the provider fallback chain consulted on retryable primary
// failures.
func Fallback(u ...string) ConvOption { return func(c *convConfig) { c.fallback = u } }

// Primary selects the provider used first for every send in this conversation
// (empty = the provider named by the model id). The Fallback chain still applies.
func Primary(u string) ConvOption { return func(c *convConfig) { c.primary = u } }

// ThinkingBudget enables extended thinking with the given token budget
// (0 = disabled, the default).
func ThinkingBudget(tokens int64) ConvOption {
	return func(c *convConfig) { c.thinkingBudget = tokens }
}

// Temperature sets the sampling temperature default.
func Temperature(t float64) ConvOption { return func(c *convConfig) { c.temperature = &t } }

// TopK sets the top-K sampling default.
func TopK(k int64) ConvOption { return func(c *convConfig) { c.topK = &k } }

// MaxTokens sets the default output cap (16384 when unset).
func MaxTokens(n int64) ConvOption { return func(c *convConfig) { c.maxTokens = n } }

// System sets the system prompt blocks. Prompt-cache breakpoints are
// applied internally at request assembly per the conversation's CachePolicy —
// callers never set cache_control themselves. System is immutable for the
// conversation's life.
func System(blocks []msg.Text) ConvOption {
	return func(c *convConfig) { c.system = blocks }
}

// SystemText is the single-block convenience form of System.
func SystemText(s string) ConvOption {
	return System([]msg.Text{{Text: s}})
}

// WithTrimPolicy sets the prompt-too-large trim policy (default: keep the
// first message + the most recent within a halving 300KB byte budget).
func WithTrimPolicy(p TrimPolicy) ConvOption { return func(c *convConfig) { c.trim = p } }

// AuthorKind stamps turn provenance ('human' | 'agent' | 'system').
func AuthorKind(k string) ConvOption { return func(c *convConfig) { c.authorKind = k } }

// WithRateLimitPolicy sets the per-conversation rate-limit retry behaviour.
// Zero values mean "use defaults" (10 retries, 10s base, 300s cap).
func WithRateLimitPolicy(p RateLimitPolicy) ConvOption {
	return func(c *convConfig) { c.rateLimit = p }
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

// WithCompaction enables context compaction with p's policy (zero fields take
// their documented defaults). Each application resolves and stores its own
// defaulted copy: an unresolved zero policy would mean "window unknown, no
// tail cap", not "defaults".
func WithCompaction(p CompactionPolicy) ConvOption {
	return func(c *convConfig) {
		d := p.withDefaults()
		c.compaction = &d
	}
}

// OnCompaction registers fn to observe compaction boundaries. It is not safe to
// call concurrently with a send on the same Conversation.
func (conv *Conversation) OnCompaction(fn func(CompactionEvent)) { conv.onCompact = fn }

// Conversation loads or creates a conversation. With WithStore it is
// DB-backed (captured, resumable); without, it degrades to an in-memory
// history with identical loop semantics.
func (c *Client) Conversation(ctx context.Context, opts ...ConvOption) (*Conversation, error) {
	cfg := convConfig{maxTokens: 16384, trim: defaultTrimPolicy{}, authorKind: "agent"}
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

	// Resolve the model once: a "<family>-latest" alias pins to a concrete id
	// for the whole conversation so the cached prefix can't drift with catalog
	// refreshes.
	resolved, err := routing.ResolveModel(c.catalog, c.defaultModel, cfg.model)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	cfg.model = resolved

	if c.capture == nil {
		// Store-less (in-memory) conversation: full loop semantics, no
		// capture and no Resume. ID is local-only.
		id := "mem-" + randomID()
		if c.mem != nil {
			c.mem.Ensure(id)
		}
		return &Conversation{client: c, ID: id, cfg: cfg}, nil
	}
	ref := capture.ConversationRef{
		OriginEntrypoint: cfg.entrypoint, DrivenBy: string(store.DrivenByServer),
		OwnerUserID: cfg.ownerUserID, Persona: cfg.persona, Model: cfg.model, Name: cfg.name, ExternalRef: cfg.externalRef,
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
func UserText(s string) []msg.Block {
	return []msg.Block{msg.Text{Text: s}}
}

// UserImage is one image attached to a user message.
//
// Data is RAW bytes, not base64 — the encoding is this package's business, and
// a caller holding base64 has almost certainly decoded it from somewhere to
// re-encode it here.
type UserImage struct {
	MediaType string
	Data      []byte
}

// UserContent builds a native user message payload from text plus attached
// images. Images come FIRST: Anthropic's own guidance is that a model attends
// better to an image that precedes the text asking about it, and the common
// shape here is "here is a screenshot" followed by a question about it.
//
// Text may be empty when the images are the whole message; an empty text block
// is omitted rather than sent, since a content block with no content is a
// 400 on some providers and noise on the rest.
func UserContent(text string, images []UserImage) []msg.Block {
	out := make([]msg.Block, 0, len(images)+1)
	for _, img := range images {
		if len(img.Data) == 0 {
			continue
		}
		out = append(out, msg.Image{MediaType: img.MediaType, Data: img.Data})
	}
	if text != "" {
		out = append(out, msg.Text{Text: text})
	}
	return out
}

type sendConfig struct {
	maxTokens     int64
	tools         []msg.ToolDef
	source        string
	authorUserID  string
	toolChoice    string
	streamHandler streamSink
	batchWait     func(bool)
	persisted     func(store.Appended)
}

// streamSink is the per-send stream observer: the handler sees the response
// accumulated so far after each content-bearing event.
type streamSink struct {
	native StreamHandler
}

// active reports whether this send asked to stream.
func (s streamSink) active() bool { return s.native != nil }

type SendOption func(*sendConfig)

// StreamHandler receives the response accumulated so far after each
// content-bearing stream event.
type StreamHandler func(partial *msg.Response)

// WithStreamHandler streams the response, invoking h after each event that
// carries content, with the response accumulated so far, and returns the
// accumulated message exactly as an unstreamed call would.
//
// h is never ignored: every sender streams. h is called synchronously on the
// calling goroutine: never concurrently with itself, and never after
// Send/Continue returns. No locking is required in h.
//
// IMPORTANT: h may be invoked one or more times and the call may STILL
// return an error — a stream can die after delivering content (see
// sendStreaming's doc for the delivered/attempted bookkeeping this drives).
// A consumer rendering h's output must be prepared for a torn, incomplete
// message and must not treat "h fired" as "the turn succeeded". Completion
// is signalled only by Send/Continue returning a nil error.
func WithStreamHandler(h StreamHandler) SendOption {
	return func(s *sendConfig) { s.streamHandler.native = h }
}

// WithBatchWait registers fn as the send's batch-wait bracket: it is called
// with true just before a send parks (a :batch first call handed to the
// daemon's Batcher, which blocks for the batch's whole lifetime) and with
// false when Park returns — on every path, including errors and ctx
// cancellation. Callers use it to keep a UI/status surface honest while the
// send is parked, and to make ctx cancellation observable.
func WithBatchWait(fn func(waiting bool)) SendOption {
	return func(s *sendConfig) { s.batchWait = fn }
}

// WithPersisted registers fn to receive the committed identity of the
// assistant row a Continue wrote, called once the row is stored and before
// Continue returns. A store-less conversation reports the wall-clock time of
// the append and an empty ID: it has no committed row.
func WithPersisted(fn func(store.Appended)) SendOption {
	return func(s *sendConfig) { s.persisted = fn }
}

// WithMaxTokens overrides the output cap for this send only.
func WithMaxTokens(n int64) SendOption { return func(s *sendConfig) { s.maxTokens = n } }

// WithTools attaches native tool definitions for this send. Definitions
// MUST be in a deterministic order across a conversation's sends (any stable
// order — e.g. sc's sorted-local + sorted-upstream concatenation): a
// reordered tools array silently busts the Anthropic prompt-cache prefix. The
// library cannot verify cross-call stability; prefix_hash on captured turns is
// the drift detector.
func WithTools(tools []msg.ToolDef) SendOption {
	return func(s *sendConfig) { s.tools = tools }
}

// WithSource overrides the per-turn inbound entrypoint.
func WithSource(src string) SendOption { return func(s *sendConfig) { s.source = src } }

// WithAuthorUserID stamps the per-turn author identity: a conversations.users
// id, never a username.
func WithAuthorUserID(id string) SendOption {
	return func(s *sendConfig) { s.authorUserID = id }
}

// WithToolChoice forces the named tool for this send only.
func WithToolChoice(name string) SendOption {
	return func(s *sendConfig) { s.toolChoice = name }
}

// Send appends userContent to the conversation and gets the assistant's
// reply: load history → persist the user message (write-ahead) → assemble →
// call (breaker/fallback/capture, prompt-too-large trim-retry) → persist the
// assistant message + turn. Returns the assistant response.
func (conv *Conversation) Send(ctx context.Context, userContent []msg.Block, opts ...SendOption) (*msg.Response, error) {
	if _, err := conv.AppendUser(ctx, userContent); err != nil {
		return nil, err
	}
	return conv.Continue(ctx, opts...)
}

// AppendUser persists user content as the next message row WITHOUT sending —
// the write-ahead primitive the agent loop uses to persist tool results (one
// row per result) before continuing. Consecutive user rows merge into a
// single wire message at request assembly. It returns the committed row's
// identity (see store.Appended; a store-less conversation reports the
// wall-clock time of the append).
func (conv *Conversation) AppendUser(ctx context.Context, content []msg.Block) (store.Appended, error) {
	row, err := conv.appendMessage(ctx, msg.Message{Role: msg.User, Content: content}, nil, "")
	if err != nil {
		return store.Appended{}, fmt.Errorf("llm: %w", err)
	}
	return row, nil
}

// Continue sends the conversation AS STORED — no new user message — and
// persists the assistant reply. This is the loop-turn / crash-recovery
// primitive: a Resume with a pending turn just Continues, re-issuing the
// interrupted call from persisted state.
func (conv *Conversation) Continue(ctx context.Context, opts ...SendOption) (*msg.Response, error) {
	scfg := sendConfig{maxTokens: conv.cfg.maxTokens}
	for _, opt := range opts {
		opt(&scfg)
	}

	ctx, span := conv.client.tracer.Start(conv.client.spanCtx(ctx), "llm.conversation.send",
		trace.WithAttributes(
			attribute.String("rafiki.conversation", conv.ID),
			attribute.String("gen_ai.conversation.id", conv.ID),
		))
	defer span.End()

	history, err := conv.loadHistory(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	if len(history) == 0 {
		return nil, errors.New("llm: Continue on an empty conversation (nothing to send)")
	}

	if p := conv.cfg.compaction; p != nil && len(history) >= minCompactableRows {
		used := conv.usedForCheck(history)
		// The proactive trigger stays suppressed until the working set has
		// grown past compactSuppressBelow (set by a prior failed attempt), so a
		// persistently failing summary is not retried every iteration.
		if p.shouldCompact(used) && used >= conv.compactSuppressBelow {
			ok, cerr := conv.compact(ctx, span, history, scfg, "threshold", "")
			if cerr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				conv.client.logger.Warn("compaction failed; continuing with unchanged history", "conversation", conv.ID, "error", cerr)
			}
			if !ok {
				// Any failed proactive attempt (call error, truncation, no
				// extractable summary) defers the next one until the working
				// set has grown by half the headroom buffer.
				conv.compactSuppressBelow = used + p.HeadroomBuffer/2
			} else if history, err = conv.loadHistory(ctx); err != nil {
				return nil, fmt.Errorf("llm: %w", err)
			}
		}
	}

	// The reactive overflow net: if the API rejects the request as too large and
	// compaction is available, summarise once and retry on the compacted history
	// before falling back to the destructive trim policy.
	var onOverflow func() (int, []msg.Message, []int, string, bool)
	if conv.cfg.compaction != nil && len(history) >= minCompactableRows {
		onOverflow = func() (int, []msg.Message, []int, string, bool) {
			ok, err := conv.compact(ctx, span, history, scfg, "overflow", "")
			if err != nil || !ok {
				return 0, nil, nil, "", false
			}
			h, err := conv.loadHistory(ctx)
			if err != nil {
				return 0, nil, nil, "", false
			}
			msgs, rowStart, err := mergeForRequestRows(h)
			if err != nil {
				return 0, nil, nil, "", false
			}
			return nextOrdinal(h), msgs, rowStart, conv.requestHeadID(h), true
		}
	}

	msgs, rowStart, err := mergeForRequestRows(history)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	resp, _, err := conv.sendWithTrim(ctx, span, nextOrdinal(history), msgs, rowStart, conv.requestHeadID(history), scfg, onOverflow)
	if err != nil {
		return nil, err
	}

	conv.usedTokens = int(msg.Val(resp.Usage.Input) + msg.Val(resp.Usage.CacheRead) +
		msg.Val(resp.Usage.CacheWrite) + msg.Val(resp.Usage.Output))

	row, err := conv.appendMessage(ctx, msg.Message{Role: msg.Assistant, Content: resp.Content},
		&store.AssistantMeta{Input: msg.Val(resp.Usage.Input), Output: msg.Val(resp.Usage.Output),
			StopReason: resp.StopReasonWire()}, resp.ID)
	if err != nil {
		// The turn is already captured; a failed message append must be loud —
		// resume protocol depends on message rows.
		return nil, fmt.Errorf("llm: persist assistant message: %w", err)
	}
	if scfg.persisted != nil {
		scfg.persisted(row)
	}
	return resp, nil
}

// OutputCap returns the conversation's per-turn output token budget (the
// max_tokens value sent to the upstream API). Callers can use this to
// auto-bump the cap when a turn is truncated.
func (conv *Conversation) OutputCap() int64 { return conv.cfg.maxTokens }

// History returns the stored WORKING set: the path from the conversation's head
// to its root — the agent loop's orphan-analysis input.
func (conv *Conversation) History(ctx context.Context) ([]*convpkg.Node, error) {
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

// RetractTailUser moves the conversation's head off the user row id — the
// write-ahead retraction for a first send the API rejected as too large, so the
// host can rebuild a smaller prompt and retry on the SAME conversation. The row
// itself survives as a leaf of the tree, unreachable from the working path.
// Refuses anything that is not the tail user row.
func (conv *Conversation) RetractTailUser(ctx context.Context, id string) error {
	return conv.handle().RetractHead(ctx, id)
}

// handle opens the conversation's handle. It is resolved on every call rather
// than captured once: the lease is installed after Conversation() returns (the
// engine build calls SetLease), and a handle freezes its fence at Open.
func (conv *Conversation) handle() *convpkg.Handle {
	return conv.client.convs.Open(conv.ID, convpkg.AsOwner(), convpkg.WithLease(conv.client.lease))
}

// loadHistory reads the WORKING set: the path from the conversation's head to
// its nearest root. The full tree is deliberately not read here — it includes
// the pre-compaction rows the working path no longer reaches, and only
// GetHistory/rafiki logs want that.
func (conv *Conversation) loadHistory(ctx context.Context) ([]*convpkg.Node, error) {
	return conv.handle().Working(ctx)
}

// appendMessage writes one message as the next node under the conversation's
// head. The tree decides identity, so a replayed node is matched by hash and
// returns the existing row's identity instead of writing a second copy.
func (conv *Conversation) appendMessage(ctx context.Context, message msg.Message, meta *store.AssistantMeta, providerID string) (store.Appended, error) {
	node := convpkg.NewNode{Msg: message, ProviderMessageID: providerID}
	if meta != nil {
		node.StopReason = meta.StopReason
		node.InputTokens = &meta.Input
		node.OutputTokens = &meta.Output
	}
	res, err := conv.handle().Append(ctx, convpkg.Append{FromHead: true, Nodes: []convpkg.NewNode{node}})
	if err != nil {
		return store.Appended{}, err
	}
	n := res.Nodes[len(res.Nodes)-1]
	return store.Appended{ID: n.ID, CreatedAt: n.CreatedAt, Ordinal: n.Ordinal, Depth: n.Depth}, nil
}

// SeedHistory idempotently persists msgs as the conversation's history from
// a fresh root — the bridge for hosts carrying client-side history (e.g. a
// fundi prefill, a Slack thread continuation). An existing prefix is matched by
// node hash, so only the missing suffix is inserted and a partial earlier seed
// is completed rather than duplicated. Re-seeding a prefix therefore inserts
// nothing and leaves the working set unchanged.
func (conv *Conversation) SeedHistory(ctx context.Context, msgs []msg.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	nodes := make([]convpkg.NewNode, len(msgs))
	for i, m := range msgs {
		nodes[i] = convpkg.NewNode{Msg: m}
	}
	_, err := conv.handle().Append(ctx, convpkg.Append{Root: convtree.RootKindRoot, Nodes: nodes})
	if err != nil {
		return fmt.Errorf("llm: seed history: %w", err)
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

// mergeForRequest converts stored nodes to wire messages, merging CONSECUTIVE
// user rows into one user message (per-tool tool_result rows → the single
// user message the API requires after an assistant tool_use turn). Block
// order within the merge follows row ordinal order.
//
// The merge rules live in mergeForRequestRows, which also reports where each
// wire message began; a request that does not need the row mapping calls this.
func mergeForRequest(history []*convpkg.Node) ([]msg.Message, error) {
	msgs, _, err := mergeForRequestRows(history)
	return msgs, err
}

// emptyToolUseNotice replaces a tool_use block the model emitted with no name.
// Forwarded as-is it is a 400 ("tool_calls[0].function.name must be a non-empty
// string"); as text the model sees its own malformed call and the matching
// is_error tool_result beside it.
const emptyToolUseNotice = "[model emitted a malformed tool call with empty name — see tool result for error]"

// replaceEmptyToolUses replaces assistant tool_use blocks with empty names
// with text blocks explaining the error. User messages pass through unchanged.
func replaceEmptyToolUses(m msg.Message) []msg.Block {
	if m.Role != msg.Assistant {
		return m.Content
	}
	n := 0
	for _, b := range m.Content {
		if t, ok := b.(msg.ToolUse); ok && t.Name == "" {
			n++
		}
	}
	if n == 0 {
		return m.Content
	}
	out := make([]msg.Block, 0, len(m.Content))
	for _, b := range m.Content {
		if t, ok := b.(msg.ToolUse); ok && t.Name == "" {
			out = append(out, msg.Text{Text: emptyToolUseNotice})
			continue
		}
		out = append(out, b)
	}
	return out
}

// sendWithTrim issues the call, applying the compaction overflow net and the
// trim policy on prompt-too-large (max 4 attempts). Trimming reshapes only the
// request message list — stored rows and the system/tools prefix are untouched,
// so the cached prefix survives every retry (prefix_hash equality across
// attempts is the regression test).
//
// rowStart maps reqMsgs to history positions and headID names the row the
// request ends at; together they let each attempt record WHICH rows it sent
// rather than their content (the rows are already stored). A trim is recorded
// as the contiguous range it dropped, which always begins where the anchor
// message ends — anchorEnd is read once, from the untrimmed list, because
// repeated trims drop a growing range from that same position.
//
// onOverflow, when non-nil, is given ONE chance (before any trim) to compact the
// conversation and hand back a fresh turn ordinal, message list, row mapping and
// head; it returns ok=false when it could not (too few rows, a failed summary
// call). The returned ordinal only labels the capture: the reply is appended
// under the head, which the compaction already moved.
func (conv *Conversation) sendWithTrim(ctx context.Context, span trace.Span, ordinal int,
	reqMsgs []msg.Message, rowStart []int, headID string, scfg sendConfig,
	onOverflow func() (ordinal int, msgs []msg.Message, rowStart []int, headID string, ok bool)) (*msg.Response, int, error) {
	meta := conv.sendMeta(ordinal, scfg)
	trimAttempt := 0
	compacted := false
	var anchorEnd int
	haveAnchor := len(rowStart) > 1
	if haveAnchor {
		anchorEnd = rowStart[1]
	}
	// dropTo is the history position one past the last dropped message; it is
	// meaningless unless haveTrim is set.
	var dropTo int
	haveTrim := false
	for try := 0; try < 4; try++ {
		// Fresh pointers per attempt: a retry must not alias the previous
		// attempt's range, which InsertTurnIntent dereferences at insert time.
		attemptMeta := meta
		attemptMeta.RequestHeadID = headID
		if haveAnchor && haveTrim {
			from, to := anchorEnd, dropTo
			attemptMeta.TrimFrom, attemptMeta.TrimTo = &from, &to
		}
		req := conv.assemble(reqMsgs, scfg)
		resp, delivered, err := conv.sendAttempt(ctx, attemptMeta, req, scfg.streamHandler)
		if err == nil {
			return resp, ordinal, nil
		}
		if delivered {
			// Structural trim-retry guard: at least one event already reached
			// the stream handler for THIS attempt. Retrying now would open a
			// second, independent stream for the same logical send with no way
			// for the caller to know the first was abandoned mid-flight — never
			// safe, regardless of what err turns out to be (isPromptTooLarge or
			// not). So this is checked before, and independent of, the
			// isPromptTooLarge check below.
			return nil, 0, err
		}
		if !isPromptTooLarge(err) {
			return nil, 0, err
		}
		if !compacted && onOverflow != nil {
			compacted = true
			if newOrdinal, newMsgs, newRowStart, newHead, ok := onOverflow(); ok {
				ordinal, reqMsgs, rowStart, headID = newOrdinal, newMsgs, newRowStart, newHead
				// Rebuild meta: the overflow compaction moved the ordinal, so
				// the stale meta would attribute this turn's capture to the
				// summary row's ordinal.
				meta = conv.sendMeta(ordinal, scfg)
				// The new list is a different set of rows: the old trim range
				// describes positions that are no longer part of the request.
				haveAnchor = len(rowStart) > 1
				if haveAnchor {
					anchorEnd = rowStart[1]
				}
				haveTrim = false
				continue
			}
		}
		trimmed, ok := conv.cfg.trim.Trim(reqMsgs, trimAttempt)
		if !ok {
			return nil, 0, err
		}
		span.AddEvent("trim", trace.WithAttributes(
			attribute.Int("rafiki.trim.attempt", trimAttempt),
			attribute.Int("rafiki.trim.before", len(reqMsgs)),
			attribute.Int("rafiki.trim.after", len(trimmed)),
		))
		conv.client.logger.Warn("prompt too large; trimmed history for retry",
			"conversation", conv.ID, "attempt", trimAttempt, "messages_before", len(reqMsgs), "messages_after", len(trimmed))
		if to, keptRowStart, dok := trimDropped(reqMsgs, trimmed, rowStart); dok {
			dropTo, rowStart, haveTrim = to, keptRowStart, true
		} else {
			// An unrecognised trim shape: the rows this list maps to are
			// unknown now, so no later attempt may claim a range either.
			dropTo, rowStart, haveTrim = 0, nil, false
		}
		reqMsgs = trimmed
		trimAttempt++
	}
	return nil, 0, errors.New("llm: prompt still too large after trim retries")
}

// sendMeta builds the per-send capture attribution and routing selection. It is
// the ONE literal, shared by the normal send path and the compaction summary
// call so their prefix metadata cannot drift.
func (conv *Conversation) sendMeta(ordinal int, scfg sendConfig) SendMeta {
	return SendMeta{
		ConversationID:   conv.ID,
		OriginEntrypoint: conv.cfg.entrypoint,
		DrivenBy:         store.DrivenByServer,
		OwnerUserID:      conv.cfg.ownerUserID,
		Persona:          conv.cfg.persona,
		Name:             conv.cfg.name,
		Ordinal:          ordinal, // history-derived turn ordinal: a decorative capture label and a stable batch id across a resume, NOT a message ordinal
		Source:           scfg.source,
		AuthorUserID:     scfg.authorUserID,
		AuthorKind:       conv.cfg.authorKind,
		RateLimit:        conv.cfg.rateLimit,
		OnBatchWait:      scfg.batchWait,
		Primary:          conv.cfg.primary,
		Fallback:         conv.cfg.fallback,
	}
}

// usedForCheck is the token count the proactive trigger compares against the
// window. After a response it is that response's reported total plus an
// estimate of the rows appended since it (tool results and steers not yet
// sent); before any response (fresh or resumed) it is a byte estimate of the
// working set plus the system prompt, since no usage figure exists yet.
func (conv *Conversation) usedForCheck(history []*convpkg.Node) int {
	if conv.usedTokens > 0 {
		last := -1
		for i, n := range history {
			if n.Msg.Role == msg.Assistant {
				last = i
			}
		}
		return conv.usedTokens + estimateHistoryTokens(history[last+1:])
	}
	systemLen := 0
	for _, b := range conv.cfg.system {
		systemLen += len(b.Text)
	}
	return estimateHistoryTokens(history) + systemLen/4
}

// sendAttempt issues ONE request attempt, streaming through the sink when set
// and transparently falling back to the plain (non-streaming) path otherwise —
// the caller (sendWithTrim) never branches on sender capability. delivered
// reports whether any content-bearing event reached the handler during this
// attempt; see sendStreaming's doc for why the trim-retry loop treats that as
// an absolute "never retry" signal.
func (conv *Conversation) sendAttempt(ctx context.Context, meta SendMeta, req *msg.Request, sink streamSink) (resp *msg.Response, delivered bool, err error) {
	if !sink.active() {
		resp, err = conv.client.send(ctx, meta, req, nil)
		return resp, false, err
	}
	resp, attempted, delivered, err := conv.client.sendStreaming(ctx, meta, req, sink)
	if !attempted {
		resp, err = conv.client.send(ctx, meta, req, nil)
		return resp, false, err
	}
	return resp, delivered, err
}

// assemble builds the wire request. Prompt-cache placement is the codec's:
// the request carries the conversation's CachePolicy and msganthropic.Encode
// puts the markers where the policy says.
func (conv *Conversation) assemble(reqMsgs []msg.Message, scfg sendConfig) *msg.Request {
	req := &msg.Request{
		Model:     conv.cfg.model,
		MaxTokens: scfg.maxTokens,
		Messages:  reqMsgs,
	}
	policy := conv.cfg.cache
	if policy == nil { // direct-construction (tests) safety; Conversation() always sets it
		d := DefaultCachePolicy()
		policy = &d
	}
	if len(conv.cfg.system) > 0 {
		req.System = slicesCloneText(conv.cfg.system)
	}
	if len(scfg.tools) > 0 {
		req.Tools = scfg.tools
	}
	req.ForceTool = scfg.toolChoice
	if conv.cfg.temperature != nil {
		t := *conv.cfg.temperature
		req.Temperature = &t
	}
	if conv.cfg.topK != nil {
		k := *conv.cfg.topK
		req.TopK = &k
	}
	if conv.cfg.thinkingBudget > 0 {
		req.ThinkingBudget = conv.cfg.thinkingBudget
	}
	req.Cache = *policy
	return req
}

// slicesCloneText copies the conversation's system blocks, so assembly never
// hands a caller a slice that aliases the conversation's own config.
func slicesCloneText(in []msg.Text) []msg.Text {
	out := make([]msg.Text, len(in))
	copy(out, in)
	return out
}

func nextOrdinal(history []*convpkg.Node) int {
	if len(history) == 0 {
		return 0
	}
	return history[len(history)-1].Ordinal + 1
}
