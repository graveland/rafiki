// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/openaichat"
	"go.graveland.dev/rafiki/pkg/providers"
)

// maxErrorBody caps how much of a refusal's body an APIError carries. A provider
// that answers with a whole HTML page must not put it in a log line.
const maxErrorBody = 1 << 20

// maxResponseBody bounds a non-streamed response body. It is generous on
// purpose: a refusal is classified from its first bytes, but a SUCCESS has to be
// read whole — a big tool-call input or a long transcript answer is normal.
const maxResponseBody = 64 << 20

// httpCodec is one dialect's wire: how a request becomes bytes, how a reply's
// bytes become native values, and how a refusal is classified.
type httpCodec struct {
	encode   func(*msg.Request, msg.EncodeOptions) ([]byte, error)
	decode   func([]byte) (*msg.Response, error)
	stream   func(io.ReadCloser) msg.StreamDecoder
	classify func(int, http.Header, []byte) msg.ErrorKind
	dialect  msg.Dialect
	path     string // "/v1/messages" or "/chat/completions"
}

// httpSender is the Sender for every plain HTTP dialect: the two Anthropic
// kinds and the OpenAI-compatible chat kind. Everything a kind decides is a
// field here rather than a second sender type — the codec, the static headers,
// how the credential rides the request, and whether the session header is set
// by a transport or by this sender itself.
type httpSender struct {
	baseURL string
	headers http.Header // static per-kind headers
	// auth sets the credential header: x-api-key for the anthropic kinds,
	// Authorization: Bearer for openai. nil means keyless, and no header is sent.
	auth func(*http.Request)
	// sessionHeader is the header the openai kind sets itself from
	// sessionIDFromContext; empty for the anthropic kinds, which use
	// sessionIDTransport so the id rides the context.
	sessionHeader string
	client        *http.Client
	codec         httpCodec
}

// anthropicCodec is the Messages API's wire.
func anthropicCodec() httpCodec {
	return httpCodec{
		encode:   msganthropic.Encode,
		decode:   msganthropic.DecodeResponse,
		stream:   msganthropic.NewStreamDecoder,
		classify: msganthropic.Classify,
		dialect:  msg.DialectAnthropic,
		path:     "/v1/messages",
	}
}

// openAIChatCodec is the Chat Completions wire.
func openAIChatCodec() httpCodec {
	return httpCodec{
		encode:   openaichat.Encode,
		decode:   openaichat.DecodeResponse,
		stream:   openaichat.NewStreamDecoder,
		classify: openaichat.Classify,
		dialect:  msg.DialectOpenAIChat,
		path:     "/chat/completions",
	}
}

// newAnthropicSender builds the plain Anthropic sender: anthropic-version and a
// content-type, with x-api-key when a key was resolved. sessionHeader is
// opt-in per provider — a plain KindAnthropic entry (Fireworks' compatible
// endpoint, say) wants sticky routing exactly as much as OpenRouter does when
// it declares the header — so the transport is wrapped only when one is set.
func newAnthropicSender(p providers.Provider, base, key string, rt http.RoundTripper) (*httpSender, error) {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Content-Type", "application/json")
	return &httpSender{
		baseURL: strings.TrimRight(base, "/"),
		headers: headers,
		auth:    apiKeyAuth("X-Api-Key", key),
		client:  &http.Client{Transport: headerCaptureTransport{base: sessionTransport(rt, p.SessionHeader)}},
		codec:   anthropicCodec(),
	}, nil
}

// newOpenRouterSender builds the anthropic-openrouter sender: OpenRouter's
// attribution headers, the session header (which defaults to its own
// x-session-id), and the same X-Api-Key credential the SDK sent for both
// anthropic kinds.
func newOpenRouterSender(p providers.Provider, base, key string, rt http.RoundTripper) (*httpSender, error) {
	sessionHeader := p.SessionHeader
	if sessionHeader == "" {
		// "x-session-id" is OpenRouter's implicit default, unconditional before
		// session_header was configurable — preserved here so an ad-hoc
		// Provider{Kind: KindAnthropicOpenRouter} literal keeps working exactly
		// as before. An explicit session_header always wins over this default.
		sessionHeader = "x-session-id"
	}
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Content-Type", "application/json")
	headers.Set("Referer", "https://github.com/graveland/rafiki")
	headers.Set("X-OpenRouter-Title", "rafiki")
	headers.Set("X-OpenRouter-Categories", "cli-agent")
	return &httpSender{
		baseURL: strings.TrimRight(base, "/"),
		headers: headers,
		auth:    apiKeyAuth("X-Api-Key", key),
		client:  &http.Client{Transport: headerCaptureTransport{base: sessionTransport(rt, sessionHeader)}},
		codec:   anthropicCodec(),
	}, nil
}

// NewAnthropicBearerSender builds an Anthropic Messages sender that
// authenticates with a bearer token and never sends x-api-key. It targets
// rafiki's own /v1/messages proxy, whose credential is a rafiki bearer token —
// a contract the plain Anthropic sender cannot express, because SenderForKey
// always keys on X-Api-Key. It reads no environment: the token is the only
// credential it has. An empty token sends no credential header at all.
func NewAnthropicBearerSender(baseURL, token string, rt http.RoundTripper) Sender {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Content-Type", "application/json")
	var auth func(*http.Request)
	if token != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+token) }
	}
	return &httpSender{
		baseURL: strings.TrimRight(baseURL, "/"),
		headers: headers,
		auth:    auth,
		client:  &http.Client{Transport: headerCaptureTransport{base: rt}},
		codec:   anthropicCodec(),
	}
}

// newOpenAISender builds the KindOpenAI sender: a generic OpenAI-compatible
// Chat Completions endpoint (Ollama, vLLM, Fireworks, …). There is no canonical
// default base URL for such an endpoint, so an empty one is a config error
// rather than a silent default.
//
// Unlike the Anthropic kinds this sender builds *http.Request values itself
// rather than going through a RoundTripper, so sessionIDTransport cannot wrap
// it: the session header is set from sessionIDFromContext on each request.
func newOpenAISender(p providers.Provider, key string, rt http.RoundTripper) (*httpSender, error) {
	if p.BaseURL == "" {
		return nil, fmt.Errorf("llm: provider %q: kind %q requires a base_url in providers.toml (no canonical default for OpenAI-compatible endpoints)", p.Name, p.Kind)
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	var auth func(*http.Request)
	if key != "" {
		// Keyless convention: an empty key sends no Authorization header at all —
		// never "Bearer " with an empty value. That is what lets an
		// unauthenticated OpenAI-compatible server (Ollama) work with an unset
		// api_key_env.
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+key) }
	}
	return &httpSender{
		baseURL:       strings.TrimRight(p.BaseURL, "/"),
		headers:       headers,
		auth:          auth,
		sessionHeader: p.SessionHeader,
		client:        &http.Client{Transport: headerCaptureTransport{base: rt}},
		codec:         openAIChatCodec(),
	}, nil
}

// sessionTransport wraps rt in sessionIDTransport when the provider declares a
// session header, and returns rt unchanged otherwise. headerCaptureTransport is
// always the OUTERMOST wrapper (see the constructors), so a captured request
// shows the session header the transport added.
func sessionTransport(rt http.RoundTripper, header string) http.RoundTripper {
	if header == "" {
		return rt
	}
	return sessionIDTransport{base: rt, header: header}
}

// apiKeyAuth returns the credential header setter for a header, or nil when
// there is no credential: keyless means NO header at all, never an empty one.
func apiKeyAuth(header, key string) func(*http.Request) {
	if key == "" {
		return nil
	}
	return func(req *http.Request) { req.Header.Set(header, key) }
}

// Send issues one non-streamed call.
func (s *httpSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	opts.Stream = false
	resp, err := s.do(ctx, req, opts, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if apiErr := s.errorFor(resp, body); apiErr != nil {
		return nil, apiErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("llm: %s: read response: %w", s.codec.dialect, readErr)
	}
	return s.codec.decode(body)
}

// Stream opens one streamed call. A refusal before the stream starts is the
// same APIError Send returns; the caller never sees a half-open stream.
func (s *httpSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	opts.Stream = true
	resp, err := s.do(ctx, req, opts, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the body before closing: it is where the classification lives.
		// Draining keeps the connection reusable.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		apiErr := s.errorFor(resp, body)
		if readErr != nil {
			// The refusal still happened, so it is still an APIError carrying
			// whatever status and partial body arrived. What it must NOT do is
			// read body-derived evidence out of a truncated body: classify on
			// the status alone, where ErrOther is the answer for anything the
			// status does not settle.
			apiErr.Kind = s.codec.classify(resp.StatusCode, resp.Header, nil)
		}
		return nil, apiErr
	}
	return s.codec.stream(resp.Body), nil
}

// do encodes req and posts it, returning any response whose status the caller
// must inspect.
func (s *httpSender) do(ctx context.Context, req *msg.Request, opts msg.EncodeOptions, accept string) (*http.Response, error) {
	body, err := s.codec.encode(req, opts)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+s.codec.path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: %s: build request: %w", s.codec.dialect, err)
	}
	for k, values := range s.headers {
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Accept", accept)
	if s.auth != nil {
		s.auth(httpReq)
	}
	if id, ok := sessionIDFromContext(ctx); ok && s.sessionHeader != "" {
		httpReq.Header.Set(s.sessionHeader, id)
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: post %s: %w", s.codec.dialect, s.codec.path, err)
	}
	return resp, nil
}

// errorFor builds the APIError for a non-2xx response, or nil for a success.
func (s *httpSender) errorFor(resp *http.Response, body []byte) *msg.APIError {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	stored := body
	if len(stored) > maxErrorBody {
		stored = stored[:maxErrorBody]
	}
	return &msg.APIError{
		Status:     resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       stored,
		Kind:       s.codec.classify(resp.StatusCode, resp.Header, body),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Dialect:    s.codec.dialect,
		Message:    errorMessageOf(body),
	}
}

// errorMessageOf reads an Anthropic error body's error.message.
func errorMessageOf(body []byte) string {
	var wire struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ""
	}
	return wire.Error.Message
}
