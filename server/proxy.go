package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/timescale/rafiki/routing"
	"github.com/timescale/savannah-common/go/tslogs"
)

type proxyStore interface {
	EnsureConversationByExternalRef(ctx context.Context, ref routing.ConversationRef) (string, error)
	InsertTurnIntent(ctx context.Context, t routing.TurnIntent) (turnID string, createdAt time.Time, err error)
	CompleteTurn(ctx context.Context, r routing.TurnResult) error
	FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error
}

type MessagesProxy struct {
	store       proxyStore
	auth        Authenticator // nil → anonymous capture (no owner)
	apiKey      string
	upstreamURL string // e.g. https://api.anthropic.com
	httpClient  *http.Client
	logger      *tslogs.Logger

	// Fallback (OpenRouter) target, set via setFallback. When orKey is empty
	// (the default), the proxy behaves exactly as primary-only: no breaker
	// consultation, no failover.
	orKey   string
	orURL   string
	breaker *routing.Breaker

	// Model resolution (server-owned default + -latest). catalog may be nil
	// (resolution then no-ops and the incoming model is forwarded as-is).
	catalog      *routing.ModelCatalog
	defaultModel string // e.g. "haiku-latest"

	metrics *Metrics // optional Prometheus instrumentation
}

// SetMetrics attaches Prometheus instrumentation (optional).
func (p *MessagesProxy) SetMetrics(m *Metrics) { p.metrics = m }

// latency renders a turn duration for logs, rounded to 100ms.
func latency(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }

func NewMessagesProxy(store *routing.CaptureStore, auth Authenticator, apiKey, upstreamURL, defaultModel string, catalog *routing.ModelCatalog, logger *tslogs.Logger) *MessagesProxy {
	// ResponseHeaderTimeout bounds connect + time-to-first-byte so a hung
	// upstream can't wedge the stream-copy loop forever. It does NOT cap
	// total streaming duration, so legitimate multi-minute SSE conversations
	// are unaffected — unlike http.Client.Timeout, which would cap the whole
	// stream and must not be set here.
	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	p := &MessagesProxy{auth: auth, apiKey: apiKey, upstreamURL: upstreamURL, defaultModel: defaultModel, catalog: catalog, httpClient: httpClient, logger: logger}
	if store != nil {
		p.store = store // avoid a typed-nil interface: nil *CaptureStore = capture-less
	}
	return p
}

// SetFallback enables OpenRouter failover. Until called, p.breaker is nil and
// ServeHTTP behaves exactly as primary-only (C-2 semantics).
func (p *MessagesProxy) SetFallback(orKey, orURL string, b *routing.Breaker) {
	p.orKey = orKey
	p.orURL = orURL
	p.breaker = b
}

// doUpstream forwards reqBody to url with the real key (stripping the
// caller's inbound auth) and the caller's anthropic-version/anthropic-beta
// headers. bearer selects the auth scheme: Anthropic's API takes the key in
// x-api-key, OpenRouter takes it in Authorization: Bearer.
func (p *MessagesProxy) doUpstream(ctx context.Context, url, key string, bearer bool, reqBody []byte, r *http.Request) (*http.Response, error) {
	return p.buildAndDo(ctx, url, key, bearer, reqBody, r, false)
}

// upstreamRequest is the OpenRouter variant: additionally forwards
// x-session-id (OpenRouter session pinning — sentinel relies on it; the
// Anthropic primary never sees it).
func (p *MessagesProxy) upstreamRequest(ctx context.Context, url, key string, bearer bool, reqBody []byte, r *http.Request) (*http.Response, error) {
	return p.buildAndDo(ctx, url, key, bearer, reqBody, r, true)
}

func (p *MessagesProxy) buildAndDo(ctx context.Context, url, key string, bearer bool, reqBody []byte, r *http.Request, forwardSession bool) (*http.Response, error) {
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	if forwardSession {
		if sid := r.Header.Get("x-session-id"); sid != "" {
			up.Header.Set("x-session-id", sid)
		}
	}
	up.Header.Set("Content-Type", "application/json")
	if bearer {
		up.Header.Set("Authorization", "Bearer "+key)
	} else {
		up.Header.Set("x-api-key", key)
	}
	if v := r.Header.Get("anthropic-version"); v != "" {
		up.Header.Set("anthropic-version", v)
	}
	if b := r.Header.Get("anthropic-beta"); b != "" {
		up.Header.Set("anthropic-beta", b)
	}
	return p.httpClient.Do(up)
}

// doOpenRouter rewrites the model in reqBody to its OpenRouter equivalent and
// forwards to OpenRouter. Reached both on Anthropic-primary failover and as the
// direct path for user-requested slash (OpenRouter-native) models. OpenRouter
// authenticates via Authorization: Bearer (its universal convention), not
// Anthropic's x-api-key.
func (p *MessagesProxy) doOpenRouter(ctx context.Context, reqBody []byte, r *http.Request) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(reqBody, &payload); err != nil {
		return nil, err
	}
	if m, ok := payload["model"].(string); ok {
		payload["model"] = p.catalog.OpenRouterModel(m)
	} else {
		p.logger.Warn("proxy: openrouter request has no string model; forwarding untranslated")
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := p.upstreamRequest(ctx, p.orURL, p.orKey, true /* bearer */, rewritten, r)
	return resp, err
}

// resolveModel rewrites the request body's "model" via routing.ResolveModel
// (empty -> server default, "<family>-latest" -> the catalog's concrete newest)
// and returns the possibly-rewritten body plus the resolved model. A decode/
// marshal failure is best-effort (the input is forwarded unchanged). A
// "<family>-latest" that the catalog can't resolve returns an error so the
// caller fails the request cleanly rather than forwarding an unusable alias.
func (p *MessagesProxy) resolveModel(reqBody []byte) ([]byte, string, error) {
	var body map[string]any
	if err := json.Unmarshal(reqBody, &body); err != nil {
		p.logger.Warn("proxy: model resolve decode failed", "error", err)
		return reqBody, "", nil
	}
	requested, _ := body["model"].(string)
	resolved, err := routing.ResolveModel(p.catalog, p.defaultModel, requested)
	if err != nil {
		return reqBody, "", err
	}
	if resolved == requested {
		return reqBody, resolved, nil
	}
	body["model"] = resolved
	out, err := json.Marshal(body)
	if err != nil {
		// Re-marshal failed: reqBody (still carrying `requested`) is what actually
		// goes upstream, so report `requested` — not `resolved` — to keep the
		// captured/logged model consistent with the wire.
		p.logger.Warn("proxy: model resolve marshal failed", "error", err)
		return reqBody, requested, nil
	}
	return out, resolved, nil
}

// captureRef bundles the identifiers for a begun capture turn so the failTurn /
// completion helpers don't each take four positional args. on=false means
// capture is disabled for this turn (the proxy still forwards).
type captureRef struct {
	convID    string
	turnID    string
	createdAt time.Time
	on        bool
}

func (p *MessagesProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	reqBody, model, err := p.resolveModel(reqBody)
	if err != nil {
		// An unresolvable "<family>-latest" (offline cold catalog): fail cleanly
		// before capture begins rather than forward an unusable alias upstream.
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Best-effort capture setup (never blocks the proxy on failure).
	convID, turnID, turnCreatedAt, capturing := p.beginCapture(r, reqBody, model)
	cr := captureRef{convID: convID, turnID: turnID, createdAt: turnCreatedAt, on: capturing}

	resp, upstream, err, handled := p.selectUpstream(w, r, reqBody, model, cr)
	if handled {
		return // selectUpstream already wrote the response and resolved the turn
	}
	if err != nil {
		p.failTurn(r, cr, err.Error())
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.streamAndCapture(w, r, resp, cr, upstream, model, start)
}

// selectUpstream routes the request and returns the upstream response. A slash
// (OpenRouter-native) model goes straight to OpenRouter; otherwise the breaker
// picks the Anthropic primary (failing over to OpenRouter on a retryable
// failure) or OpenRouter directly when the breaker is open. handled=true means
// it already wrote the response (a config error, not an upstream call) and
// resolved any begun turn; the caller must just return.
func (p *MessagesProxy) selectUpstream(w http.ResponseWriter, r *http.Request, reqBody []byte, model string, cr captureRef) (resp *http.Response, upstream string, err error, handled bool) {
	if strings.Contains(model, "/") {
		// Slash id = an OpenRouter-native model; route directly to OpenRouter,
		// skipping the Anthropic primary + breaker. No failover: the caller asked
		// for this specific model. doOpenRouter forwards slash ids untranslated.
		if p.orKey == "" {
			p.failTurn(r, cr, "openrouter not configured for model "+model)
			http.Error(w, "openrouter not configured for model "+model, http.StatusBadGateway)
			return nil, "", nil, true
		}
		resp, err = p.doOpenRouter(r.Context(), reqBody, r)
		return resp, "openrouter", err, false
	}
	now := time.Now()
	if p.breaker == nil || p.breaker.UsePrimary(now) {
		resp, err = p.doUpstream(r.Context(), p.upstreamURL, p.apiKey, false /* x-api-key */, reqBody, r)
		if p.breaker == nil {
			return resp, "anthropic", err, false
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		primaryFailed := routing.ClassifyFailure(status, err)
		p.breaker.RecordResult(now, primaryFailed)
		if !primaryFailed || p.orKey == "" {
			return resp, "anthropic", err, false
		}
		// Failing over to OpenRouter. Log it (mirrors core.go's in-process path) so
		// an Anthropic outage masked by a successful failover isn't invisible.
		p.logger.Warn("proxy: primary failed, failing over to openrouter", "status", status, "error", err)
		if resp != nil {
			resp.Body.Close()
		}
		resp, err = p.doOpenRouter(r.Context(), reqBody, r)
		return resp, "openrouter", err, false
	}
	if p.orKey == "" {
		p.failTurn(r, cr, "no upstream available")
		http.Error(w, "no upstream available", http.StatusBadGateway)
		return nil, "", nil, true
	}
	resp, err = p.doOpenRouter(r.Context(), reqBody, r)
	return resp, "openrouter", err, false
}

// streamAndCapture transparently streams the upstream response to the client
// while teeing a copy, then records the turn: a 4xx/5xx or a mid-stream read
// error fails it; a clean stream completes it with the reassembled canonical
// response. The per-turn "llm turn" log fires regardless of whether DB capture
// is on, so savannah-admin surfaces every proxied turn.
func (p *MessagesProxy) streamAndCapture(w http.ResponseWriter, r *http.Request, resp *http.Response, cr captureRef, upstream, model string, start time.Time) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var acc bytes.Buffer
	body := io.TeeReader(resp.Body, &acc)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	var streamErr error // a real mid-stream read error (not clean io.EOF)
	clientGone := false // client disconnected mid-stream (w.Write failed)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				p.logger.Warn("proxy: client write failed mid-stream", "conversation", cr.convID, "error", werr)
				clientGone = true
				break // client went away; record as aborted below, not complete
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				streamErr = rerr // surfaced once below as "llm turn truncated"
			}
			break
		}
	}

	elapsed := time.Since(start)
	// A 4xx/5xx upstream is a failed turn, not a clean completion; so is a
	// mid-stream read error that truncated the body. (The Messages API never
	// responds 3xx, so treating <400 as success is safe.)
	if resp.StatusCode >= 400 {
		p.logger.Warn("llm turn failed", "conversation", cr.convID, "upstream", upstream, "model", model, "status", resp.StatusCode, "latency", latency(elapsed))
		p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "upstream status "+strconv.Itoa(resp.StatusCode))
		return
	}
	if streamErr != nil {
		p.logger.Warn("llm turn truncated", "conversation", cr.convID, "upstream", upstream, "model", model, "error", streamErr, "latency", latency(elapsed))
		p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "mid-stream read error: "+streamErr.Error())
		return
	}
	if clientGone {
		// The client disconnected mid-stream: the accumulated body is a partial
		// response with undercounted usage. Record it errored, not 'complete', so
		// truncated turns don't pollute the capture store as clean completions.
		p.logger.Warn("llm turn aborted", "conversation", cr.convID, "upstream", upstream, "model", model, "reason", "client disconnected mid-stream", "latency", latency(elapsed))
		p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "client disconnected mid-stream")
		return
	}

	stop, usage, canonical, perr := routing.ParseCapturedResponse(resp.Header.Get("Content-Type"), acc.Bytes())
	if perr != nil {
		// A stream we could not parse must not be persisted as a clean
		// completion — record it errored (the client already got the bytes).
		p.logger.Warn("llm turn capture-parse failed", "conversation", cr.convID, "upstream", upstream, "model", model, "error", perr)
		p.metrics.ObserveTurn(upstream, "error", "anthropic", time.Since(start), routing.CapturedUsage{})
		p.failTurn(r, cr, "capture parse failed: "+perr.Error())
		return
	}
	p.logger.Info("llm turn",
		"conversation", cr.convID, "upstream", upstream, "model", model,
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"cache_read_tokens", usage.CacheReadTokens, "cache_creation_tokens", usage.CacheCreationTokens,
		"stop_reason", stop, "latency", latency(elapsed))
	p.metrics.ObserveTurn(upstream, "complete", "anthropic", elapsed, usage)
	if !cr.on {
		return
	}
	// Detached: a mid-stream client disconnect cancels r.Context(), but the
	// capture write happens after streaming ends and must still complete so the
	// turn isn't stranded 'pending'.
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if cerr := p.store.CompleteTurn(capCtx, routing.TurnResult{
		TurnID: cr.turnID, CreatedAt: cr.createdAt, Response: canonical, StopReason: stop, Upstream: upstream,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens,
		LatencyMS: int(elapsed.Milliseconds()),
	}); cerr != nil {
		p.logger.Warn("proxy capture: complete-turn failed", "conversation", cr.convID, "error", cerr)
		// Don't leave the turn stranded 'pending' on a completion write failure;
		// mark it errored so it isn't a false orphan for the sweep.
		p.failTurn(r, cr, "complete-turn failed: "+cerr.Error())
	}
}

// failTurn resolves a begun capture turn as errored (best-effort) so a handled
// error path never strands the row 'pending'. No-op when capture is off. Uses a
// detached context: r.Context() may already be canceled (client disconnect).
func (p *MessagesProxy) failTurn(r *http.Request, cr captureRef, reason string) {
	if !cr.on {
		return
	}
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if ferr := p.store.FailTurn(capCtx, cr.turnID, cr.createdAt, reason); ferr != nil {
		p.logger.Warn("proxy capture: fail-turn failed", "conversation", cr.convID, "error", ferr)
	}
}

// beginCapture correlates the session + write-aheads the request. Best-effort:
// returns capturing=false (proxy still forwards) on any failure.
func (p *MessagesProxy) beginCapture(r *http.Request, reqBody []byte, model string) (convID, turnID string, createdAt time.Time, capturing bool) {
	if p.store == nil {
		return "", "", time.Time{}, false // capture-less (no store configured)
	}
	owner := ""
	if p.auth != nil {
		if id := p.auth.Identify(r); id != nil {
			owner = id.Username
		}
	}
	// Source lets a single proxy serve multiple entrypoints (Claude Code, a TUI, a
	// slack bot): the client stamps X-Rafiki-Source; default to the conversation's
	// own entrypoint. These are interactive, human-driven sessions.
	//
	// Conversation-row population on this (client-driven) path: owner comes
	// from the Authenticator (token name / host identity; NULL when
	// anonymous); model is backfilled by the first turn's resolved model
	// (first-seen wins); external_ref is X-Rafiki-Session — WITHOUT the
	// header each request DELIBERATELY becomes its own conversation (no
	// implicit correlation heuristics: guessing by owner or time would merge
	// genuinely separate sessions); persona is intentionally NULL — personas
	// are a launcher/library concept the proxy never sees (an
	// X-Rafiki-Persona header is the future hook if that changes).
	source := r.Header.Get("X-Rafiki-Source")
	if source == "" {
		source = "claude"
	}
	convID, err := p.store.EnsureConversationByExternalRef(r.Context(), routing.ConversationRef{
		OriginEntrypoint: source, DrivenBy: "client",
		Owner: owner, ExternalRef: r.Header.Get("X-Rafiki-Session"),
	})
	if err != nil {
		p.logger.Warn("proxy capture: ensure-conversation failed", "error", err)
		return "", "", time.Time{}, false
	}
	// Ordering on the client path is by created_at; ordinal is unused here.
	turnID, createdAt, err = p.store.InsertTurnIntent(r.Context(), routing.TurnIntent{
		ConversationID: convID, Ordinal: 0, Model: model, Request: reqBody,
		Source: source, Author: owner, AuthorKind: "human", PrefixHash: routing.PrefixHash(reqBody),
	})
	if err != nil {
		p.logger.Warn("proxy capture: insert-intent failed", "conversation", convID, "error", err)
		return convID, "", time.Time{}, false
	}
	return convID, turnID, createdAt, true
}
