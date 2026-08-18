// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// OpenAIUpstream is one configured /v1/chat/completions target.
type OpenAIUpstream struct {
	Name    string // captured as conversation_turn.upstream
	BaseURL string // e.g. https://openrouter.ai/api/v1
	APIKey  string
}

// OpenAIRoute maps a model-id prefix to an upstream name.
type OpenAIRoute struct {
	Prefix   string
	Upstream string
}

// ChatCompletionsProxy is the OpenAI-face pass-through: forward + tee +
// capture with protocol='openai'. No cross-upstream failover in v1 (the
// default upstream, OpenRouter, self-routes); the breaker stays an
// Anthropic-face concern. The tee mutates nothing at all — not even the
// model (no resolution on this face).
type ChatCompletionsProxy struct {
	store      proxyStore
	auth       Authenticator
	upstreams  map[string]OpenAIUpstream
	routes     []OpenAIRoute
	defaultUp  string
	httpClient *http.Client
	logger     *slog.Logger
	metrics    *Metrics
}

func NewChatCompletionsProxy(cs *capture.CaptureStore, auth Authenticator, upstreams []OpenAIUpstream, routes []OpenAIRoute, defaultUpstream string, logger *slog.Logger) *ChatCompletionsProxy {
	ups := make(map[string]OpenAIUpstream, len(upstreams))
	for _, u := range upstreams {
		ups[u.Name] = u
	}
	var ps proxyStore
	if cs != nil {
		ps = cs
	}
	return &ChatCompletionsProxy{
		store: ps, auth: auth, upstreams: ups, routes: routes, defaultUp: defaultUpstream,
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
		logger: logger,
	}
}

// SetMetrics attaches Prometheus instrumentation (optional).
func (p *ChatCompletionsProxy) SetMetrics(m *Metrics) { p.metrics = m }

func (p *ChatCompletionsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// The auth middleware honours X-Rafiki-Token on every face it wraps, but
	// passthrough is an Anthropic-face feature: this one authenticates to each
	// upstream with that upstream's own key. Serving the request anyway would
	// bill the daemon while the caller believed it was self-billing, so refuse
	// rather than silently ignore the intent.
	if PassthroughCredential(r.Context()) != "" {
		http.Error(w, "passthrough auth is not supported on the OpenAI-compatible face; it applies to /v1/messages only", http.StatusBadRequest)
		return
	}
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	model := modelOfBody(reqBody)
	up, ok := p.selectUpstream(model)
	if !ok {
		http.Error(w, "no upstream configured for model "+model, http.StatusBadGateway)
		return
	}

	cr := p.beginCapture(r, reqBody, model)

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimSuffix(up.BaseURL, "/")+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		p.failTurn(r, cr, err.Error())
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	sid := r.Header.Get("x-session-id")
	if sid == "" {
		sid = cr.convID // fall back to rafiki's own conversation id so pinning is always present
	}
	if sid != "" {
		upReq.Header.Set("x-session-id", sid) // OpenRouter session pinning
	}

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		p.failTurn(r, cr, err.Error())
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.streamAndCapture(w, r, resp, cr, up.Name, model, start)
}

func (p *ChatCompletionsProxy) selectUpstream(model string) (OpenAIUpstream, bool) {
	name := p.defaultUp
	for _, route := range p.routes {
		if strings.HasPrefix(model, route.Prefix) {
			name = route.Upstream
			break
		}
	}
	up, ok := p.upstreams[name]
	return up, ok
}

func (p *ChatCompletionsProxy) streamAndCapture(w http.ResponseWriter, r *http.Request, resp *http.Response, cr captureRef, upstream, model string, start time.Time) {
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
	var streamErr error
	clientGone := false
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				p.logger.Warn("openai proxy: client write failed mid-stream", "conversation", cr.convID, "error", werr)
				clientGone = true
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				streamErr = rerr
			}
			break
		}
	}

	elapsed := time.Since(start)
	switch {
	case resp.StatusCode >= 400:
		p.logger.Warn("llm turn failed", "conversation", cr.convID, "upstream", upstream, "model", model, "protocol", "openai", "status", resp.StatusCode, "latency", latency(elapsed))
		p.metrics.ObserveTurn(upstream, "error", "openai", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "upstream status "+strconv.Itoa(resp.StatusCode))
		return
	case streamErr != nil:
		p.logger.Warn("llm turn truncated", "conversation", cr.convID, "upstream", upstream, "model", model, "protocol", "openai", "error", streamErr)
		p.metrics.ObserveTurn(upstream, "error", "openai", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "mid-stream read error: "+streamErr.Error())
		return
	case clientGone:
		p.logger.Warn("llm turn aborted", "conversation", cr.convID, "upstream", upstream, "model", model, "protocol", "openai", "reason", "client disconnected mid-stream")
		p.metrics.ObserveTurn(upstream, "error", "openai", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "client disconnected mid-stream")
		return
	}

	finish, usage, canonical, perr := parseOpenAIResponse(resp.Header.Get("Content-Type"), acc.Bytes())
	if perr != nil {
		p.logger.Warn("llm turn capture-parse failed", "conversation", cr.convID, "upstream", upstream, "model", model, "protocol", "openai", "error", perr)
		p.metrics.ObserveTurn(upstream, "error", "openai", elapsed, routing.CapturedUsage{})
		p.failTurn(r, cr, "capture parse failed: "+perr.Error())
		return
	}
	p.logger.Info("llm turn",
		"conversation", cr.convID, "upstream", upstream, "model", model, "protocol", "openai",
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"cache_read_tokens", usage.CacheReadTokens,
		"cache_pct", cachePct(usage),
		"finish_reason", finish, "latency", latency(elapsed))
	p.metrics.ObserveTurn(upstream, "complete", "openai", elapsed, usage)
	if !cr.on {
		return
	}
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if cerr := p.store.CompleteTurn(capCtx, capture.TurnResult{
		TurnID: cr.turnID, CreatedAt: cr.createdAt, Model: usage.Model, Response: canonical, StopReason: finish, Upstream: upstream,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens,
		LatencyMS: int(elapsed.Milliseconds()),
	}); cerr != nil {
		p.logger.Warn("openai proxy capture: complete-turn failed", "conversation", cr.convID, "error", cerr)
		p.failTurn(r, cr, "complete-turn failed: "+cerr.Error())
	}
}

func (p *ChatCompletionsProxy) beginCapture(r *http.Request, reqBody []byte, model string) captureRef {
	if p.store == nil {
		return captureRef{}
	}
	ownerUserID := ""
	if p.auth != nil {
		if id := p.auth.Identify(r); id != nil {
			ownerUserID = id.UserID
		}
	}
	source := r.Header.Get("X-Rafiki-Source")
	if source == "" {
		source = "openai"
	}
	convID, err := p.store.EnsureConversationByExternalRef(r.Context(), capture.ConversationRef{
		OriginEntrypoint: source, DrivenBy: string(store.DrivenByClient),
		OwnerUserID: ownerUserID, ExternalRef: r.Header.Get("X-Rafiki-Session"),
	})
	if err != nil {
		p.logger.Warn("openai proxy capture: ensure-conversation failed", "error", err)
		return captureRef{}
	}
	turnID, createdAt, err := p.store.InsertTurnIntent(r.Context(), capture.TurnIntent{
		ConversationID: convID, Ordinal: 0, Model: model, Request: reqBody,
		Source: source, AuthorUserID: ownerUserID, AuthorKind: "human",
		PrefixHash: routing.PrefixHash(reqBody), Protocol: string(store.ProtocolOpenAI),
	})
	if err != nil {
		p.logger.Warn("openai proxy capture: insert-intent failed", "conversation", convID, "error", err)
		return captureRef{convID: convID}
	}
	return captureRef{convID: convID, turnID: turnID, createdAt: createdAt, on: true}
}

func (p *ChatCompletionsProxy) failTurn(r *http.Request, cr captureRef, reason string) {
	if !cr.on {
		return
	}
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if ferr := p.store.FailTurn(capCtx, cr.turnID, cr.createdAt, reason); ferr != nil {
		p.logger.Warn("openai proxy capture: fail-turn failed", "conversation", cr.convID, "error", ferr)
	}
}

func modelOfBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// parseOpenAIResponse extracts finish_reason + usage and returns the
// canonical body to persist: an SSE stream is reassembled into a
// chat.completion-shaped JSON with content AND tool_calls deltas accumulated
// (send stream_options.include_usage for usage on streams); a JSON body
// passes through unchanged. Parse failures are ERRORS — the caller fails the
// turn rather than persisting garbage as a clean completion.
func parseOpenAIResponse(contentType string, body []byte) (string, routing.CapturedUsage, []byte, error) {
	if !strings.Contains(contentType, "text/event-stream") {
		var m struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return "", routing.CapturedUsage{}, nil, fmt.Errorf("undecodable JSON response: %w", err)
		}
		finish := ""
		if len(m.Choices) > 0 {
			finish = m.Choices[0].FinishReason
		}
		u := m.Usage.captured()
		u.Model = m.Model
		return finish, u, body, nil
	}

	var (
		finish    string
		usage     openAIUsage
		content   strings.Builder
		id        string
		model     string
		chunks    int
		toolCalls []*openAIToolCall
	)
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", routing.CapturedUsage{}, nil, fmt.Errorf("undecodable SSE chunk: %w", err)
		}
		chunks++
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			for _, tc := range c.Delta.ToolCalls {
				for tc.Index >= len(toolCalls) {
					toolCalls = append(toolCalls, &openAIToolCall{})
				}
				agg := toolCalls[tc.Index]
				if tc.ID != "" {
					agg.ID = tc.ID
				}
				if tc.Type != "" {
					agg.Type = tc.Type
				}
				if tc.Function.Name != "" {
					agg.Function.Name = tc.Function.Name
				}
				agg.Function.Arguments += tc.Function.Arguments
			}
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	if err := sc.Err(); err != nil {
		return "", routing.CapturedUsage{}, nil, fmt.Errorf("scan SSE stream: %w", err)
	}
	if chunks == 0 {
		return "", routing.CapturedUsage{}, nil, errors.New("no decodable SSE chunks in stream")
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	canonical, err := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion", "model": model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	})
	if err != nil {
		return "", routing.CapturedUsage{}, nil, fmt.Errorf("marshal canonical response: %w", err)
	}
	u := usage.captured()
	u.Model = model
	return finish, u, canonical, nil
}

// openAIToolCall is an accumulated tool_calls delta (arguments arrive as
// string fragments across chunks).
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func (u openAIUsage) captured() routing.CapturedUsage {
	out := routing.CapturedUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptDetails != nil {
		out.CacheReadTokens = u.PromptDetails.CachedTokens
	}
	return out
}
