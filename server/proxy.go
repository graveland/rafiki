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

	"golang.org/x/net/http2"

	"github.com/timescale/rafiki/routing"
	"github.com/timescale/savannah-common/go/tslogs"
)

type proxyStore interface {
	EnsureConversationByExternalRef(ctx context.Context, ref routing.ConversationRef) (string, error)
	InsertTurnIntent(ctx context.Context, t routing.TurnIntent) (turnID string, createdAt time.Time, err error)
	CompleteTurn(ctx context.Context, r routing.TurnResult) error
	FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error
	DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) (int, error)
	AppendResponseMessage(ctx context.Context, convID, turnID string, createdAt time.Time, ordinal int, canonical []byte, in, out int64, stopReason string) error
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
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	// h2 keepalive: without ReadIdleTimeout a silently dead upstream TCP
	// connection (NAT/LB drop) leaves the stream-copy Read hanging forever —
	// the client sees a stalled stream with no error. With it, a quiet
	// connection is pinged and torn down in ~40s, surfacing a real read error.
	// Anthropic's SSE ping events reset the timer, so healthy long turns are
	// unaffected.
	if h2, err := http2.ConfigureTransports(transport); err == nil {
		h2.ReadIdleTimeout = 30 * time.Second
		h2.PingTimeout = 10 * time.Second
	} else {
		logger.Warn("proxy: h2 transport configure failed; no upstream keepalive pings", "error", err)
	}
	httpClient := &http.Client{Transport: transport}
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
	// Pinned model lines get their provider preferences injected; a
	// caller-supplied provider object always wins over the pin.
	if m, ok := payload["model"].(string); ok {
		if _, has := payload["provider"]; !has {
			if prefs, pinned := routing.ProviderPrefsFor(m); pinned {
				payload["provider"] = prefs
			}
		}
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
	convID      string
	turnID      string
	createdAt   time.Time
	on          bool
	reqBody     []byte // decomposed post-stream in streamAndCapture (see beginCapture)
	prefixHash  string
	nextOrdinal int // ordinal for the assistant response message (= request message count)
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
	cr := p.beginCapture(r, reqBody, model)

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
	// A 4xx/5xx is a small, non-streamed error body — handle it before touching
	// the streaming path so we can buffer, surface the provider's real message
	// to the client, and capture it. (The Messages API never responds 3xx, so
	// treating <400 as a stream to tee is safe.)
	if resp.StatusCode >= 400 {
		p.handleUpstreamError(w, r, resp, cr, upstream, model, start)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var acc bytes.Buffer
	body := io.TeeReader(resp.Body, &acc)
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		// Without a Flusher every SSE event (including keep-alive pings) sits in
		// net/http's buffer until it fills — the client's stall detector will
		// abort long turns. Loud because it means a middleware broke streaming.
		p.logger.Warn("proxy: ResponseWriter is not an http.Flusher; SSE responses will buffer", "conversation", cr.convID)
	}
	buf := make([]byte, 16*1024)
	var streamErr error    // a real mid-stream read error (not clean io.EOF)
	clientGone := false    // client disconnected mid-stream (w.Write failed)
	lastByte := time.Now() // when the last upstream byte arrived (starts at header time)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			lastByte = time.Now()
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
	user := p.requestUser(r)
	// A mid-stream read error that truncated the body is a failed turn, not a
	// clean completion. (Upstream 4xx/5xx is handled before streaming begins, in
	// handleUpstreamError.)
	if streamErr != nil {
		// last_byte_ago disambiguates the two stall stories: ~0 means bytes were
		// still flowing when the stream died (proxy→client hop or client gave up
		// early); tens of seconds means upstream went quiet first and the client's
		// stall detector correctly bailed.
		stall := latency(time.Since(lastByte))
		if r.Context().Err() != nil {
			// Inbound request context canceled — the client hung up (stall-detector
			// abort, Esc, disconnect) and the cancel propagated into our upstream
			// read. Upstream is not implicated.
			p.logger.Warn("llm turn aborted", "conversation", cr.convID, "user", user, "upstream", upstream, "model", model, "reason", "client canceled", "bytes", acc.Len(), "last_byte_ago", stall, "latency", latency(elapsed))
			p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
			p.failTurn(r, cr, "client canceled mid-stream (last upstream byte "+stall+" earlier)")
			return
		}
		p.logger.Warn("llm turn truncated", "conversation", cr.convID, "user", user, "upstream", upstream, "model", model, "error", streamErr, "bytes", acc.Len(), "last_byte_ago", stall, "latency", latency(elapsed))
		p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "mid-stream read error: "+streamErr.Error())
		return
	}
	if clientGone {
		// The client disconnected mid-stream: the accumulated body is a partial
		// response with undercounted usage. Record it errored, not 'complete', so
		// truncated turns don't pollute the capture store as clean completions.
		p.logger.Warn("llm turn aborted", "conversation", cr.convID, "user", user, "upstream", upstream, "model", model, "reason", "client disconnected mid-stream", "bytes", acc.Len(), "last_byte_ago", latency(time.Since(lastByte)), "latency", latency(elapsed))
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
		"conversation", cr.convID, "user", user, "upstream", upstream, "model", model,
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
		TurnID: cr.turnID, CreatedAt: cr.createdAt, Response: nil, StopReason: stop, Upstream: upstream,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens,
		LatencyMS: int(elapsed.Milliseconds()),
	}); cerr != nil {
		p.logger.Warn("proxy capture: complete-turn failed", "conversation", cr.convID, "error", cerr)
		// Don't leave the turn stranded 'pending' on a completion write failure;
		// mark it errored so it isn't a false orphan for the sweep.
		p.failTurn(r, cr, "complete-turn failed: "+cerr.Error())
		return
	}
	// Best-effort: decompose the request into conversation_message rows (the
	// DB I/O deliberately deferred from beginCapture — see its comment) before
	// appending the response, so request messages at ordinals 0..N-1 exist
	// before the response lands at cr.nextOrdinal (=N). Neither call fails the
	// turn — CompleteTurn above already recorded it complete.
	if _, derr := p.store.DecomposeRequest(capCtx, cr.convID, cr.turnID, cr.createdAt, cr.reqBody, cr.prefixHash); derr != nil {
		p.logger.Warn("proxy capture: decompose request failed", "conversation", cr.convID, "error", derr)
	}
	if aerr := p.store.AppendResponseMessage(capCtx, cr.convID, cr.turnID, cr.createdAt,
		cr.nextOrdinal, canonical, usage.InputTokens, usage.OutputTokens, stop); aerr != nil {
		p.logger.Warn("proxy capture: append response message failed", "conversation", cr.convID, "error", aerr)
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

// maxCapturedErrorBody bounds how much of an upstream error body is folded into
// a failed turn's reason. Provider 4xx bodies are small JSON; the cap only
// guards against a pathological upstream, not normal errors.
const maxCapturedErrorBody = 8 << 10 // 8 KiB

// boundedErrorBody returns a trimmed, size-bounded copy of an upstream error
// body suitable for storing and logging. Empty in → empty out.
func boundedErrorBody(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	if len(b) > maxCapturedErrorBody {
		// Cut on a valid UTF-8 boundary: a raw byte offset can split a multi-byte
		// rune, and the invalid tail would break a UTF8 log sink or a text/jsonb
		// column insert (dropping the very capture this preserves).
		return strings.ToValidUTF8(string(b[:maxCapturedErrorBody]), "") + "…(truncated)"
	}
	return string(b)
}

// decomposeRequestBestEffort decomposes cr.reqBody into conversation_message
// rows on a fail path, where no CompleteTurn ran (so no capCtx exists). Uses a
// detached, bounded context: the client may already be gone. No-op when capture
// is off.
func (p *MessagesProxy) decomposeRequestBestEffort(r *http.Request, cr captureRef) {
	if !cr.on {
		return
	}
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if _, derr := p.store.DecomposeRequest(capCtx, cr.convID, cr.turnID, cr.createdAt, cr.reqBody, cr.prefixHash); derr != nil {
		p.logger.Warn("proxy capture: decompose request (failed turn) failed", "conversation", cr.convID, "error", derr)
	}
}

// handleUpstreamError forwards a non-2xx upstream response to the client and
// records the failed turn. It buffers the (small) error body so it can (a)
// surface the provider's real message — OpenRouter wraps the actionable detail
// in error.metadata.raw, which a client showing only the top-level
// error.message otherwise never sees — and (b) store it, making the failure
// diagnosable from the capture store alone.
func (p *MessagesProxy) handleUpstreamError(w http.ResponseWriter, r *http.Request, resp *http.Response, cr captureRef, upstream, model string, start time.Time) {
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		// The error body is small; a read failure means the upstream connection
		// dropped mid-delivery. Note it so the failed turn isn't a silent,
		// bodyless mystery, and still forward whatever bytes did arrive.
		p.logger.Warn("proxy: reading upstream error body failed", "conversation", cr.convID, "error", rerr, "bytes", len(raw))
	}
	elapsed := time.Since(start)
	user := p.requestUser(r)

	clientBody, surfaced := raw, false
	if s, ok := surfaceProviderError(raw); ok {
		clientBody, surfaced = s, true
	}
	for k, vs := range resp.Header {
		// Content-Length is recomputed below. When we rewrote the body to
		// plaintext, an upstream Content-Encoding would mislabel it, so drop that
		// too — but only then; a passthrough body keeps upstream's encoding.
		if strings.EqualFold(k, "Content-Length") || (surfaced && strings.EqualFold(k, "Content-Encoding")) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(clientBody)))
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(clientBody); werr != nil {
		p.logger.Warn("proxy: client write failed on error response", "conversation", cr.convID, "error", werr)
	}

	// Store the ORIGINAL upstream body (the richest form) on the turn, not the
	// client-facing rewrite.
	errBody := boundedErrorBody(raw)
	reason := "upstream status " + strconv.Itoa(resp.StatusCode)
	if rerr != nil {
		reason += " (error body read failed: " + rerr.Error() + ")"
	}
	if errBody != "" {
		reason += ": " + errBody
	}
	p.logger.Warn("llm turn failed", "conversation", cr.convID, "user", user, "upstream", upstream, "model", model, "status", resp.StatusCode, "body", errBody, "latency", latency(elapsed))
	p.metrics.ObserveTurn(upstream, "error", "anthropic", elapsed, routing.CapturedUsage{})
	p.failTurn(r, cr, reason)
	// Decompose the request even on failure so the turn is inspectable — the
	// messages/prefix that triggered the error are exactly what you need to see
	// (especially for a request-caused 4xx). The success path decomposes after
	// CompleteTurn; here there is no completion, only the request.
	p.decomposeRequestBestEffort(r, cr)
}

// requestUser resolves the caller's username for turn logs, or "<unknown>".
func (p *MessagesProxy) requestUser(r *http.Request) string {
	if p.auth != nil {
		if id := p.auth.Identify(r); id != nil && id.Username != "" {
			return id.Username
		}
	}
	return "<unknown>"
}

// surfaceProviderError unwraps OpenRouter's provider-error envelope so the
// client sees the real reason. On a provider rejection OpenRouter returns
//
//	{"error":{"message":"Provider returned error","metadata":{"raw":"<json>",
//	 "provider_name":"OpenAI"}}}
//
// where the actionable detail (e.g. an unsupported value and the allowed set)
// lives in metadata.raw. Lift raw.error.message into the top-level
// error.message, preserving the rest of the body, so a client that displays
// error.message (Claude Code) shows it. Returns (nil, false) when the body is
// not such an envelope — the caller then passes the original through unchanged.
func surfaceProviderError(body []byte) ([]byte, bool) {
	var env struct {
		Error struct {
			Metadata struct {
				Raw          string `json:"raw"`
				ProviderName string `json:"provider_name"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Metadata.Raw == "" {
		return nil, false
	}
	var inner struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(env.Error.Metadata.Raw), &inner); err != nil || inner.Error.Message == "" {
		return nil, false
	}
	detail := inner.Error.Message
	if pn := env.Error.Metadata.ProviderName; pn != "" {
		detail = pn + ": " + detail
	}
	// Rewrite only the top-level error.message, preserving the rest of the
	// structure the client expects. UseNumber keeps integer fields exact through
	// the round-trip (a plain map decode would widen them to float64).
	var generic map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, false
	}
	errObj, ok := generic["error"].(map[string]any)
	if !ok {
		return nil, false
	}
	errObj["message"] = detail
	out, err := json.Marshal(generic)
	if err != nil {
		return nil, false
	}
	return out, true
}

// beginCapture correlates the session and write-aheads the request
// (InsertTurnIntent — deliberately synchronous, before the upstream call).
// Decomposing reqBody into conversation_message rows is DB I/O and is
// deferred to streamAndCapture's post-stream block so it never gates the
// proxied request; here we only need nextOrdinal, computed with a cheap,
// I/O-free parse. cr.on=false (proxy still forwards) on any failure setting
// up the turn itself.
func (p *MessagesProxy) beginCapture(r *http.Request, reqBody []byte, model string) captureRef {
	if p.store == nil {
		return captureRef{} // capture-less (no store configured)
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
		return captureRef{}
	}
	// Ordering on the client path is by created_at; ordinal is unused here.
	// Request is no longer stored verbatim on the turn row — DecomposeRequest
	// below covers it as conversation_message rows.
	prefixHash := routing.PrefixHash(reqBody)
	turnID, createdAt, err := p.store.InsertTurnIntent(r.Context(), routing.TurnIntent{
		ConversationID: convID, Ordinal: 0, Model: model, Request: nil,
		Source: source, Author: owner, AuthorKind: "human", PrefixHash: prefixHash,
	})
	if err != nil {
		p.logger.Warn("proxy capture: insert-intent failed", "conversation", convID, "error", err)
		return captureRef{convID: convID}
	}
	return captureRef{
		convID: convID, turnID: turnID, createdAt: createdAt, on: true,
		reqBody: reqBody, prefixHash: prefixHash, nextOrdinal: countRequestMessages(reqBody),
	}
}

// countRequestMessages returns len(reqBody.messages) via a cheap, allocation-
// light unmarshal (no DB I/O) — just enough to know the ordinal the assistant
// response will land on. Decomposition itself (the actual conversation_message
// INSERTs) happens later, off the latency-critical path; 0 on any parse error.
func countRequestMessages(reqBody []byte) int {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &body); err != nil {
		return 0
	}
	return len(body.Messages)
}
